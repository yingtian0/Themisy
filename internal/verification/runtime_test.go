package verification

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"strings"
	"testing"
	"themisy/adapters/ecs"
	"themisy/internal/domain"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"time"
)

const runtimeCluster = "arn:aws:ecs:ap-northeast-1:111122223333:cluster/test"
const runtimeService = "arn:aws:ecs:ap-northeast-1:111122223333:service/test/service"
const runtimeDefinition = "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/service:1"
const runtimeTask = "arn:aws:ecs:ap-northeast-1:111122223333:task/test/task1"

type runtimeAPI struct {
	writes  int
	pending bool
}

func (a *runtimeAPI) DescribeTaskDefinition(context.Context, *awsecs.DescribeTaskDefinitionInput, ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error) {
	return &awsecs.DescribeTaskDefinitionOutput{TaskDefinition: &types.TaskDefinition{TaskDefinitionArn: aws.String(runtimeDefinition), Status: types.TaskDefinitionStatusActive, ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app"), Image: aws.String("example/app@sha256:" + strings.Repeat("a", 64))}}}}, nil
}
func (a *runtimeAPI) DescribeServices(context.Context, *awsecs.DescribeServicesInput, ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error) {
	state := types.DeploymentRolloutStateCompleted
	if a.pending {
		state = types.DeploymentRolloutStateInProgress
	}
	return &awsecs.DescribeServicesOutput{Services: []types.Service{{ServiceArn: aws.String(runtimeService), ClusterArn: aws.String(runtimeCluster), TaskDefinition: aws.String(runtimeDefinition), Status: aws.String("ACTIVE"), DesiredCount: 1, RunningCount: 1, Deployments: []types.Deployment{{Status: aws.String("PRIMARY"), TaskDefinition: aws.String(runtimeDefinition), DesiredCount: 1, RunningCount: 1, RolloutState: state}}}}}, nil
}
func (a *runtimeAPI) ListTasks(context.Context, *awsecs.ListTasksInput, ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error) {
	return &awsecs.ListTasksOutput{TaskArns: []string{runtimeTask}}, nil
}
func (a *runtimeAPI) DescribeTasks(context.Context, *awsecs.DescribeTasksInput, ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	return &awsecs.DescribeTasksOutput{Tasks: []types.Task{{TaskArn: aws.String(runtimeTask), ClusterArn: aws.String(runtimeCluster), TaskDefinitionArn: aws.String(runtimeDefinition), Group: aws.String("service:service"), DesiredStatus: aws.String("RUNNING"), LastStatus: aws.String("RUNNING"), HealthStatus: types.HealthStatusHealthy, Containers: []types.Container{{Name: aws.String("app"), TaskArn: aws.String(runtimeTask), LastStatus: aws.String("RUNNING"), HealthStatus: types.HealthStatusHealthy, ImageDigest: aws.String("sha256:" + strings.Repeat("a", 64))}}}}}, nil
}
func (a *runtimeAPI) UpdateService(context.Context, *awsecs.UpdateServiceInput, ...func(*awsecs.Options)) (*awsecs.UpdateServiceOutput, error) {
	a.writes++
	return nil, errors.New("runtime observer must not write")
}

type runtimeCredentialBroker struct {
	calls   int
	request credentials.Request
	err     error
}

func (b *runtimeCredentialBroker) Acquire(_ context.Context, r credentials.Request) (credentials.Credential, error) {
	b.calls++
	b.request = r
	return credentials.New(map[string][]byte{"token": []byte("secret-canary")}, time.Now().Add(time.Hour)), b.err
}

type runtimeSink struct {
	events []domain.AuditEvent
	err    error
	hook   func(context.Context)
}

func (s *runtimeSink) AppendAuditContext(ctx context.Context, e domain.AuditEvent) error {
	s.events = append(s.events, e)
	if s.hook != nil {
		s.hook(ctx)
	}
	return s.err
}
func runtimeFixture(t *testing.T) (*RuntimeCollector, *runtimeAPI, *runtimeCredentialBroker, *runtimeSink, RuntimeAuditRequest) {
	t.Helper()
	client := &runtimeAPI{}
	digest := "sha256:" + strings.Repeat("a", 64)
	reader, err := ecs.New(ecs.Config{Targets: map[ecs.TargetKey]ecs.Target{{Service: "service", Environment: "staging"}: {Region: "ap-northeast-1", ClusterARN: runtimeCluster, ServiceARN: runtimeService, TaskDefinitions: map[string]ecs.TaskDefinition{digest: {ARN: runtimeDefinition, ExpectedTaskDefinitions: []string{runtimeDefinition}, ExpectedContainerDigests: map[string]string{"app": digest}}}}}}, func(context.Context, string, credentials.Credential) (ecs.API, error) { return client, nil })
	if err != nil {
		t.Fatal(err)
	}
	broker := &runtimeCredentialBroker{}
	sink := &runtimeSink{}
	collector, err := NewRuntimeCollector("tenant", reader, broker, sink, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return collector, client, broker, sink, RuntimeAuditRequest{RunID: "run", GrantID: "grant", Request: adapter.ReconcileRequest{Target: adapter.Target{Service: "service", Environment: "staging"}, ArtifactDigest: digest, IdempotencyKey: "observation"}}
}
func TestRuntimeCollectorCommitsSeparateObservationAndRefusesImportedTrust(t *testing.T) {
	c, client, broker, sink, request := runtimeFixture(t)
	snapshot, err := c.Collect(context.Background(), request)
	if err != nil || snapshot.RequireFreshRuntime(time.Now()) != nil || client.writes != 0 || len(sink.events) != 1 || broker.request.Purpose != credentials.PurposeVerify || broker.request.Provider != credentials.ProviderAWS {
		t.Fatal("collection failed", err)
	}
	event := sink.events[0]
	record := event.Details["record"].(RuntimeRecord)
	hash, _ := runtimeRecordHash(record)
	if event.Action != "runtime.observe" || record.Hash != hash || hash != snapshot.Hash() || record.GrantID != "grant" || event.Result != "PASS" {
		t.Fatal("audit binding")
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "arn:aws:") {
		t.Fatal("provider/credential detail stored")
	}
	record.Observation.ReasonCode = "changed"
	if snapshot.Record().Observation.ReasonCode == "changed" {
		t.Fatal("mutable record alias")
	}
	var imported RuntimeSnapshot
	if json.Unmarshal(raw, &imported) == nil || imported.RequireFreshRuntime(time.Now()) == nil {
		t.Fatal("JSON minted trust")
	}
	expiry := snapshot.Record().Observation.ObservedAt.Add(time.Minute)
	if snapshot.RequireFreshRuntime(expiry) == nil {
		t.Fatal("expiry equality accepted")
	}
	client.pending = true
	pending, err := c.Collect(context.Background(), request)
	if err != nil || pending.RequireFreshRuntime(time.Now()) == nil || pending.Record().Observation.Status != adapter.VerificationInconclusive || client.writes != 0 {
		t.Fatal("pending promoted", err)
	}
}
func TestRuntimeCollectorFailuresReturnNoSnapshot(t *testing.T) {
	for _, mode := range []string{"unknown-target", "credential", "audit", "cancel", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			c, client, broker, sink, r := runtimeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "unknown-target":
				r.Request.Target.Service = "unknown"
			case "credential":
				broker.err = errors.New("secret-canary")
			case "audit":
				sink.err = errors.New("response lost")
			case "cancel":
				sink.hook = func(context.Context) { cancel() }
			case "expiry":
				sink.hook = func(context.Context) { c.now = func() time.Time { return time.Now().Add(time.Hour) } }
			}
			if s, e := c.Collect(ctx, r); e == nil || s.Hash() != "" || client.writes != 0 {
				t.Fatal("failure returned snapshot")
			}
			if mode == "unknown-target" && broker.calls != 0 {
				t.Fatal("unknown target acquired credential")
			}
			if len(sink.events) > 1 {
				t.Fatal("ambiguous append retried")
			}
		})
	}
}
