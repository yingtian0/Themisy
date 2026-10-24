package ecs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func TestDeployResolvesOnlyTrustedECSResources(t *testing.T) {
	fixture := newFixture(t)
	deployment, err := fixture.adapter.Deploy(context.Background(), fixture.request, credentials.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.client.updateCalls != 1 || aws.ToString(fixture.client.updateInput.Cluster) != fixture.target.ClusterARN || aws.ToString(fixture.client.updateInput.Service) != fixture.target.ServiceARN || aws.ToString(fixture.client.updateInput.TaskDefinition) != fixture.definition.ARN {
		t.Fatalf("update=%#v calls=%d", fixture.client.updateInput, fixture.client.updateCalls)
	}
	if fixture.client.retryMaxAttempts != 1 {
		t.Fatalf("retry max attempts=%d", fixture.client.retryMaxAttempts)
	}
	if strings.Contains(deployment.ExternalExecutionID, "arn:aws") {
		t.Fatalf("external ID leaked AWS ARN: %s", deployment.ExternalExecutionID)
	}
}

func TestDeployRejectsUnknownLogicalTargetAndUnexpectedCurrentTaskDefinition(t *testing.T) {
	fixture := newFixture(t)
	request := fixture.request
	request.Target.Service = "arn:aws:ecs:ap-northeast-1:999999999999:service/attacker"
	if _, err := fixture.adapter.Deploy(context.Background(), request, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorTerminal) || fixture.client.updateCalls != 0 {
		t.Fatalf("unknown target err=%v calls=%d", err, fixture.client.updateCalls)
	}
	request = fixture.request
	fixture.client.current = "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment:999"
	if _, err := fixture.adapter.Deploy(context.Background(), request, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorTerminal) || fixture.client.updateCalls != 0 {
		t.Fatalf("unexpected task definition err=%v calls=%d", err, fixture.client.updateCalls)
	}
}

func TestDeployRejectsArtifactDigestWithoutTrustedTaskDefinition(t *testing.T) {
	fixture := newFixture(t)
	request := fixture.request
	request.ArtifactDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := fixture.adapter.Deploy(context.Background(), request, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorTerminal) || fixture.client.updateCalls != 0 {
		t.Fatalf("unmapped digest err=%v calls=%d", err, fixture.client.updateCalls)
	}
}

func TestAmbiguousUpdateIsUnknownAndNeverRetried(t *testing.T) {
	fixture := newFixture(t)
	fixture.client.updateErr = context.DeadlineExceeded
	if _, err := fixture.adapter.Deploy(context.Background(), fixture.request, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorUnknown) {
		t.Fatalf("err=%v", err)
	}
	if fixture.client.updateCalls != 1 {
		t.Fatalf("UpdateService calls=%d, want one", fixture.client.updateCalls)
	}
}

func TestReconcileObservesActualTaskDefinitionWithoutWrite(t *testing.T) {
	fixture := newFixture(t)
	reconcile := adapter.ReconcileRequest{IdempotencyKey: fixture.request.IdempotencyKey, Target: fixture.request.Target, ArtifactDigest: fixture.request.ArtifactDigest, DispatchedAt: time.Now()}
	result, err := fixture.adapter.Reconcile(context.Background(), reconcile, credentials.Credential{})
	if err != nil || result.Status != adapter.ReconcilePending || fixture.client.updateCalls != 0 {
		t.Fatalf("pending result=%#v err=%v calls=%d", result, err, fixture.client.updateCalls)
	}
	fixture.client.current = fixture.definition.ARN
	result, err = fixture.adapter.Reconcile(context.Background(), reconcile, credentials.Credential{})
	if err != nil || result.Status != adapter.ReconcileSucceeded || fixture.client.updateCalls != 0 {
		t.Fatalf("success result=%#v err=%v calls=%d", result, err, fixture.client.updateCalls)
	}
}

func TestStatusReadsAllowListedServiceAndDoesNotWrite(t *testing.T) {
	fixture := newFixture(t)
	fixture.client.status = "ACTIVE"
	fixture.client.desiredCount = 3
	fixture.client.runningCount = 2
	fixture.client.pendingCount = 1
	fixture.client.deploymentState = "IN_PROGRESS"
	status, err := fixture.adapter.Status(context.Background(), adapter.StatusRequest{Target: fixture.request.Target}, credentials.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if status.ServiceStatus != "ACTIVE" || status.TaskDefinition != fixture.target.RollbackTaskDefinition || status.DesiredCount != 3 || status.RunningCount != 2 || status.PendingCount != 1 || len(status.Deployments) != 1 || status.Deployments[0].RolloutState != "IN_PROGRESS" {
		t.Fatalf("status=%#v", status)
	}
	if fixture.client.describeCalls != 1 || fixture.client.updateCalls != 0 {
		t.Fatalf("describe=%d update=%d", fixture.client.describeCalls, fixture.client.updateCalls)
	}
	reads := fixture.client.describeCalls
	request := adapter.StatusRequest{Target: adapter.Target{Service: "arn:aws:ecs:attacker", Environment: "production"}}
	if _, err := fixture.adapter.Status(context.Background(), request, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorTerminal) || fixture.client.describeCalls != reads || fixture.client.updateCalls != 0 {
		t.Fatalf("untrusted target err=%v describe=%d update=%d", err, fixture.client.describeCalls, fixture.client.updateCalls)
	}
}

type ecsFixture struct {
	adapter    *Adapter
	client     *fakeECS
	target     Target
	definition TaskDefinition
	request    adapter.DeployRequest
}

func newFixture(t *testing.T) ecsFixture {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	previous := "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment:41"
	definition := TaskDefinition{ARN: "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment:42", ExpectedContainerDigests: map[string]string{"app": digest}, ExpectedTaskDefinitions: []string{previous}}
	target := Target{Region: "ap-northeast-1", ClusterARN: "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod", ServiceARN: "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment", TaskDefinitions: map[string]TaskDefinition{digest: definition}, RollbackTaskDefinition: previous, RollbackContainerDigests: map[string]string{"app": digest}}
	client := &fakeECS{current: previous, status: "ACTIVE", desiredCount: 1, runningCount: 1, deploymentState: "COMPLETED"}
	instance, err := New(Config{Targets: map[TargetKey]Target{{Service: "payment-api", Environment: "production"}: target}}, func(_ context.Context, region string, _ credentials.Credential) (API, error) {
		if region != target.Region {
			return nil, errors.New("wrong region")
		}
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	instance.now = func() time.Time { return time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC) }
	return ecsFixture{adapter: instance, client: client, target: target, definition: definition, request: adapter.DeployRequest{Target: adapter.Target{Service: "payment-api", Environment: "production"}, ArtifactDigest: digest, IdempotencyKey: "grant-1/payment-api"}}
}

type fakeECS struct {
	definitionHook  func(*awsecs.DescribeTaskDefinitionOutput)
	definitionErr   error
	definitionCalls int
	serviceHook     func(*awsecs.DescribeServicesOutput)
	listHook        func(*awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error)
	taskHook        func(*awsecs.DescribeTasksOutput)
	listCalls       int
	taskCalls       int

	current          string
	status           string
	desiredCount     int32
	runningCount     int32
	pendingCount     int32
	deploymentState  string
	updateInput      *awsecs.UpdateServiceInput
	updateErr        error
	updateCalls      int
	describeCalls    int
	retryMaxAttempts int
}

func (f *fakeECS) DescribeServices(_ context.Context, _ *awsecs.DescribeServicesInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error) {
	f.describeCalls++
	output := &awsecs.DescribeServicesOutput{Services: []types.Service{{
		ServiceArn: aws.String("arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment"), ClusterArn: aws.String("arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod"), Status: aws.String(f.status), TaskDefinition: aws.String(f.current),
		DesiredCount: f.desiredCount, RunningCount: f.runningCount, PendingCount: f.pendingCount,
		Deployments: []types.Deployment{{TaskDefinition: aws.String(f.current), Status: aws.String("PRIMARY"), RolloutState: types.DeploymentRolloutState(f.deploymentState), DesiredCount: f.desiredCount, RunningCount: f.runningCount}},
	}}}
	if f.serviceHook != nil {
		f.serviceHook(output)
	}
	return output, nil
}

func (f *fakeECS) UpdateService(_ context.Context, input *awsecs.UpdateServiceInput, options ...func(*awsecs.Options)) (*awsecs.UpdateServiceOutput, error) {
	f.updateCalls++
	f.updateInput = input
	configured := awsecs.Options{}
	for _, option := range options {
		option(&configured)
	}
	f.retryMaxAttempts = configured.RetryMaxAttempts
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.current = aws.ToString(input.TaskDefinition)
	return &awsecs.UpdateServiceOutput{Service: &types.Service{TaskDefinition: input.TaskDefinition}}, nil
}

func (f *fakeECS) ListTasks(_ context.Context, input *awsecs.ListTasksInput, _ ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error) {
	f.listCalls++
	if f.listHook != nil {
		return f.listHook(input)
	}
	return &awsecs.ListTasksOutput{TaskArns: []string{"arn:aws:ecs:ap-northeast-1:111122223333:task/prod/task-1"}}, nil
}
func (f *fakeECS) DescribeTasks(_ context.Context, input *awsecs.DescribeTasksInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	f.taskCalls++
	arn := input.Tasks[0]
	output := &awsecs.DescribeTasksOutput{Tasks: []types.Task{{TaskArn: aws.String(arn), ClusterArn: input.Cluster, Group: aws.String("service:payment"), TaskDefinitionArn: aws.String(f.current), DesiredStatus: aws.String("RUNNING"), LastStatus: aws.String("RUNNING"), HealthStatus: types.HealthStatusHealthy, Containers: []types.Container{{Name: aws.String("app"), TaskArn: aws.String(arn), LastStatus: aws.String("RUNNING"), HealthStatus: types.HealthStatusHealthy, ImageDigest: aws.String("sha256:" + strings.Repeat("a", 64))}}}}}
	if f.taskHook != nil {
		f.taskHook(output)
	}
	return output, nil
}

func TestAdapterOwnsNestedOperatorConfiguration(t *testing.T) {
	f := newFixture(t)
	key := TargetKey{Service: f.request.Target.Service, Environment: f.request.Target.Environment}
	config := Config{Targets: map[TargetKey]Target{key: f.target}}
	instance, err := New(config, func(context.Context, string, credentials.Credential) (API, error) { return f.client, nil })
	if err != nil {
		t.Fatal(err)
	}
	originalDefinition := f.definition.ARN
	// Change every externally owned map/slice after construction.
	definition := config.Targets[key].TaskDefinitions[f.request.ArtifactDigest]
	definition.ExpectedTaskDefinitions[0] = "untrusted-predecessor"
	definition.ExpectedContainerDigests["app"] = "sha256:" + strings.Repeat("b", 64)
	config.Targets[key].TaskDefinitions[f.request.ArtifactDigest] = TaskDefinition{ARN: "untrusted-definition"}
	config.Targets[key].RollbackContainerDigests["app"] = "sha256:" + strings.Repeat("b", 64)
	delete(config.Targets, key)
	if _, err := instance.Deploy(context.Background(), f.request, credentials.Credential{}); err != nil {
		t.Fatal("caller changed live deployment mapping", err)
	}
	if f.client.updateCalls != 1 || aws.ToString(f.client.updateInput.TaskDefinition) != originalDefinition {
		t.Fatal("caller redirected deployment")
	}
	rollback := adapter.RollbackRequest{Target: f.request.Target, IdempotencyKey: "rollback-key", OriginalDeployment: adapter.Deployment{ExternalExecutionID: "previous"}}
	if _, err := instance.Rollback(context.Background(), rollback, credentials.Credential{}); err != nil {
		t.Fatal("caller changed rollback container expectations", err)
	}
	if f.client.updateCalls != 2 || aws.ToString(f.client.updateInput.TaskDefinition) != f.target.RollbackTaskDefinition {
		t.Fatal("caller redirected rollback")
	}
	request := f.request
	request.ArtifactDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := instance.Deploy(context.Background(), request, credentials.Credential{}); err == nil || f.client.updateCalls != 2 {
		t.Fatal("unmapped digest reached provider write")
	}
}

func (f *fakeECS) DescribeTaskDefinition(_ context.Context, input *awsecs.DescribeTaskDefinitionInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error) {
	f.definitionCalls++
	output := &awsecs.DescribeTaskDefinitionOutput{TaskDefinition: &types.TaskDefinition{TaskDefinitionArn: input.TaskDefinition, Status: types.TaskDefinitionStatusActive, ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app"), Image: aws.String("example/app@sha256:" + strings.Repeat("a", 64))}}}}
	if f.definitionHook != nil {
		f.definitionHook(output)
	}
	return output, f.definitionErr
}
func TestTaskDefinitionContentMustMatchBeforeDeployAndRollback(t *testing.T) {
	for _, operation := range []string{"deploy", "rollback"} {
		for _, mode := range []string{"wrong-arn", "inactive", "mutable-image", "wrong-digest", "missing", "extra", "duplicate", "provider-error"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				f := newFixture(t)
				f.client.definitionHook = func(o *awsecs.DescribeTaskDefinitionOutput) {
					d := o.TaskDefinition
					switch mode {
					case "wrong-arn":
						d.TaskDefinitionArn = aws.String("foreign")
					case "inactive":
						d.Status = types.TaskDefinitionStatusInactive
					case "mutable-image":
						d.ContainerDefinitions[0].Image = aws.String("example/app:latest")
					case "wrong-digest":
						d.ContainerDefinitions[0].Image = aws.String("example/app@sha256:" + strings.Repeat("b", 64))
					case "missing":
						d.ContainerDefinitions = nil
					case "extra":
						d.ContainerDefinitions = append(d.ContainerDefinitions, types.ContainerDefinition{Name: aws.String("sidecar"), Image: aws.String("extra")})
					case "duplicate":
						d.ContainerDefinitions = append(d.ContainerDefinitions, d.ContainerDefinitions[0])
					}
				}
				if mode == "provider-error" {
					f.client.definitionErr = errors.New("raw provider secret")
				}
				var err error
				if operation == "deploy" {
					_, err = f.adapter.Deploy(context.Background(), f.request, credentials.Credential{})
				} else {
					f.client.current = f.definition.ARN
					_, err = f.adapter.Rollback(context.Background(), adapter.RollbackRequest{Target: f.request.Target, IdempotencyKey: "rollback", OriginalDeployment: adapter.Deployment{ExternalExecutionID: "prior"}}, credentials.Credential{})
				}
				if err == nil || f.client.updateCalls != 0 || f.client.definitionCalls != 1 {
					t.Fatal("unverified definition reached write")
				}
			})
		}
	}
}
