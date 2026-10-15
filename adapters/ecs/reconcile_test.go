package ecs

import (
	"context"
	"testing"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func TestAmbiguousRollbackReconcilesWithoutRepeatingWrite(t *testing.T) {
	for _, applied := range []bool{false, true} {
		name := "not_applied"
		if applied {
			name = "applied"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.client.current = fixture.definition.ARN
			fixture.client.updateErr = context.DeadlineExceeded
			rollback := adapter.RollbackRequest{Target: fixture.request.Target, OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original-deploy"}, IdempotencyKey: "rollback-key"}
			if _, err := fixture.adapter.Rollback(context.Background(), rollback, credentials.Credential{}); !adapter.IsClass(err, adapter.ErrorUnknown) {
				t.Fatalf("rollback err=%v, want ambiguous outcome", err)
			}
			if fixture.client.updateCalls != 1 {
				t.Fatalf("rollback writes=%d, want one", fixture.client.updateCalls)
			}
			want := adapter.ReconcilePending
			if applied {
				fixture.client.current = fixture.target.RollbackTaskDefinition
				want = adapter.ReconcileSucceeded
			}
			request := adapter.ReconcileRequest{Operation: adapter.ReconcileRollback, Target: rollback.Target, ArtifactDigest: fixture.request.ArtifactDigest, OriginalDeployment: rollback.OriginalDeployment, IdempotencyKey: rollback.IdempotencyKey}
			result, err := fixture.adapter.Reconcile(context.Background(), request, credentials.Credential{})
			if err != nil || result.Status != want || fixture.client.updateCalls != 1 {
				t.Fatalf("result=%#v err=%v writes=%d want=%s with no additional write", result, err, fixture.client.updateCalls, want)
			}
		})
	}
}

func TestReconcileRollbackUsesTrustedRollbackDefinitionWithoutWrite(t *testing.T) {
	for _, state := range []struct {
		name    string
		current func(ecsFixture) string
		want    adapter.ReconcileStatus
	}{
		{"rollback_not_applied", func(f ecsFixture) string { return f.definition.ARN }, adapter.ReconcilePending},
		{"rollback_applied", func(f ecsFixture) string { return f.target.RollbackTaskDefinition }, adapter.ReconcileSucceeded},
		{"unrecognized_definition", func(ecsFixture) string { return "untrusted-task-definition" }, adapter.ReconcileFailed},
	} {
		t.Run(state.name, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.client.current = state.current(fixture)
			request := adapter.ReconcileRequest{
				Operation:          adapter.ReconcileRollback,
				Target:             fixture.request.Target,
				ArtifactDigest:     fixture.request.ArtifactDigest,
				OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original-deploy"},
				IdempotencyKey:     "rollback-key",
			}
			result, err := fixture.adapter.Reconcile(context.Background(), request, credentials.Credential{})
			if err != nil || result.Status != state.want {
				t.Fatalf("result=%#v err=%v want=%s", result, err, state.want)
			}
			if fixture.client.updateCalls != 0 {
				t.Fatalf("reconciliation issued %d provider writes", fixture.client.updateCalls)
			}
		})
	}
}

func TestReconcileRollbackDoesNotRequireDeploymentArtifact(t *testing.T) {
	fixture := newFixture(t)
	request := adapter.ReconcileRequest{Operation: adapter.ReconcileRollback, Target: fixture.request.Target, OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original-deploy"}, IdempotencyKey: "rollback-key"}
	result, err := fixture.adapter.Reconcile(context.Background(), request, credentials.Credential{})
	if err != nil || result.Status != adapter.ReconcileSucceeded || fixture.client.updateCalls != 0 {
		t.Fatalf("result=%#v err=%v writes=%d", result, err, fixture.client.updateCalls)
	}
}

func TestReconcileRejectsInvalidOperationAndRollbackTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ecsFixture, *adapter.ReconcileRequest)
	}{
		{"unknown_operation", func(_ *ecsFixture, r *adapter.ReconcileRequest) { r.Operation = "unknown" }},
		{"missing_original_execution", func(_ *ecsFixture, r *adapter.ReconcileRequest) { r.OriginalDeployment = adapter.Deployment{} }},
		{"unknown_target", func(_ *ecsFixture, r *adapter.ReconcileRequest) { r.Target.Service = "untrusted" }},
		{"missing_rollback_definition", func(f *ecsFixture, _ *adapter.ReconcileRequest) {
			key := TargetKey{Service: f.request.Target.Service, Environment: f.request.Target.Environment}
			target := f.adapter.config.Targets[key]
			target.RollbackTaskDefinition = ""
			f.adapter.config.Targets[key] = target
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			request := adapter.ReconcileRequest{Operation: adapter.ReconcileRollback, Target: fixture.request.Target, ArtifactDigest: fixture.request.ArtifactDigest, OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original-deploy"}, IdempotencyKey: "rollback-key"}
			test.change(&fixture, &request)
			result, err := fixture.adapter.Reconcile(context.Background(), request, credentials.Credential{})
			if !adapter.IsClass(err, adapter.ErrorTerminal) || result.Status == adapter.ReconcileSucceeded || fixture.client.updateCalls != 0 {
				t.Fatalf("result=%#v err=%v writes=%d", result, err, fixture.client.updateCalls)
			}
		})
	}
}

func TestReconcileExplicitDeployRetainsDeploymentTarget(t *testing.T) {
	fixture := newFixture(t)
	fixture.client.current = fixture.definition.ARN
	request := adapter.ReconcileRequest{Operation: adapter.ReconcileDeploy, Target: fixture.request.Target, ArtifactDigest: fixture.request.ArtifactDigest, IdempotencyKey: fixture.request.IdempotencyKey}
	result, err := fixture.adapter.Reconcile(context.Background(), request, credentials.Credential{})
	if err != nil || result.Status != adapter.ReconcileSucceeded || fixture.client.updateCalls != 0 {
		t.Fatalf("result=%#v err=%v writes=%d", result, err, fixture.client.updateCalls)
	}
}
