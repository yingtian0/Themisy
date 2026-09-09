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
	updateInput      *awsecs.UpdateServiceInput
	updateErr        error
	updateCalls      int
	retryMaxAttempts int
}

func (f *fakeECS) DescribeServices(_ context.Context, _ *awsecs.DescribeServicesInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error) {
	return &awsecs.DescribeServicesOutput{Services: []types.Service{{TaskDefinition: aws.String(f.current)}}}, nil
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
