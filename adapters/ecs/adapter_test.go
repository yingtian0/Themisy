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
	definition := TaskDefinition{ARN: "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment:42", ExpectedTaskDefinitions: []string{previous}}
	target := Target{Region: "ap-northeast-1", ClusterARN: "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod", ServiceARN: "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment", TaskDefinitions: map[string]TaskDefinition{digest: definition}, RollbackTaskDefinition: previous}
	client := &fakeECS{current: previous}
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
	return &awsecs.DescribeServicesOutput{Services: []types.Service{{
		Status: aws.String(f.status), TaskDefinition: aws.String(f.current),
		DesiredCount: f.desiredCount, RunningCount: f.runningCount, PendingCount: f.pendingCount,
		Deployments: []types.Deployment{{TaskDefinition: aws.String(f.current), Status: aws.String("PRIMARY"), RolloutState: types.DeploymentRolloutState(f.deploymentState), DesiredCount: f.desiredCount, RunningCount: f.runningCount}},
	}}}, nil
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
	return &awsecs.UpdateServiceOutput{Service: &types.Service{TaskDefinition: input.TaskDefinition}}, nil
}
