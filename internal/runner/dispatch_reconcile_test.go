package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"themisy/internal/store"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

type reconcileSDKProbe struct {
	sdkProbe
	result       adapter.ReconcileResult
	reconcileErr error
	requests     []adapter.ReconcileRequest
}

func (p *reconcileSDKProbe) Reconcile(_ context.Context, request adapter.ReconcileRequest, _ credentials.Credential) (adapter.ReconcileResult, error) {
	p.requests = append(p.requests, request)
	return p.result, p.reconcileErr
}

func TestSDKDispatcherReconcileRequiresExplicitSuccess(t *testing.T) {
	finished := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		status     adapter.ReconcileStatus
		adapterErr error
		found      bool
		wantErr    bool
	}{
		{name: "succeeded", status: adapter.ReconcileSucceeded, found: true},
		{name: "pending", status: adapter.ReconcilePending},
		{name: "not found", status: adapter.ReconcileNotFound},
		{name: "failed", status: adapter.ReconcileFailed, wantErr: true},
		{name: "empty status", wantErr: true},
		{name: "unrecognized status", status: "COMPLETED", wantErr: true},
		{name: "adapter error overrides success", status: adapter.ReconcileSucceeded, adapterErr: errors.New("read failed"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &reconcileSDKProbe{result: adapter.ReconcileResult{Status: tc.status, Deployment: adapter.Deployment{ExternalExecutionID: "external-1", FinishedAt: finished}}, reconcileErr: tc.adapterErr}
			dispatcher := &SDKDispatcher{Adapter: provider}
			result, found, err := dispatcher.Reconcile(context.Background(), AdapterRequest{Action: protocol.Action{Capability: protocol.CapabilityDeploy}}, Credential{})
			if found != tc.found || (err != nil) != tc.wantErr {
				t.Fatalf("result=%#v found=%v err=%v", result, found, err)
			}
			if tc.found {
				if result.ExternalExecutionID != "external-1" || !result.CompletedAt.Equal(finished) {
					t.Fatalf("lost deployment: %#v", result)
				}
			} else if result != (AdapterResult{}) {
				t.Fatalf("unresolved deployment leaked as result: %#v", result)
			}
			if tc.adapterErr != nil && !errors.Is(err, tc.adapterErr) {
				t.Fatalf("adapter error lost: %v", err)
			}
		})
	}
}

func TestRunnerSDKReconcileInvalidStatusRemainsUnknown(t *testing.T) {
	for _, status := range []adapter.ReconcileStatus{"", "COMPLETED"} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newRunnerFixture(t)
			provider := &reconcileSDKProbe{sdkProbe: sdkProbe{err: &adapter.Error{Class: adapter.ErrorUnknown, Operation: "deploy", Err: errors.New("timeout")}}, result: adapter.ReconcileResult{Status: status, Deployment: adapter.Deployment{ExternalExecutionID: "unverified-execution", FinishedAt: fixture.now}}}
			dispatcher := &SDKDispatcher{Adapter: provider}
			fixture.runner.Adapter = dispatcher
			result, err := fixture.runner.Execute(context.Background(), fixture.grant)
			if err == nil || result.Status != protocol.ResultUnknown {
				t.Fatalf("execute=%#v err=%v", result, err)
			}
			if err := fixture.runner.Reconcile(context.Background(), dispatcher, 10); err != nil {
				t.Fatal(err)
			}
			record, err := fixture.journal.GetRunnerAction(context.Background(), fixture.grant.TenantID, fixture.grant.RunnerGroup, fixture.grant.Nonce)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != store.RunnerActionUnknown || record.Result.Status != protocol.ResultUnknown || record.Result.ReasonCode != "RECONCILIATION_UNRESOLVED" || record.Result.ExternalExecutionID != "" {
				t.Fatalf("invalid status marked resolved: %#v", record)
			}
			if provider.deployCalls != 1 {
				t.Fatalf("reconciliation dispatched a new write: %d", provider.deployCalls)
			}
		})
	}
}

func TestSDKDispatcherReconcilePreservesOperation(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		capability protocol.Capability
		operation  adapter.ReconcileOperation
		original   string
		wantErr    bool
	}{
		{name: "deploy", capability: protocol.CapabilityDeploy, operation: adapter.ReconcileDeploy},
		{name: "rollback", capability: protocol.CapabilityRollback, operation: adapter.ReconcileRollback, original: "original-deployment"},
		{name: "unsupported", capability: "release.delete", wantErr: true},
		{name: "missing capability", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &reconcileSDKProbe{result: adapter.ReconcileResult{Status: adapter.ReconcilePending}}
			dispatcher := &SDKDispatcher{Adapter: provider}
			request := AdapterRequest{IdempotencyKey: "retry-key", DispatchedAt: now, Target: protocol.Target{Service: "service", Environment: "staging"}, Action: protocol.Action{Capability: tc.capability, ArtifactDigest: "sha256:artifact", ExternalExecutionID: "original-deployment"}}
			_, found, err := dispatcher.Reconcile(context.Background(), request, Credential{})
			if found || (err != nil) != tc.wantErr {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if tc.wantErr {
				if len(provider.requests) != 0 {
					t.Fatal("unsupported action reached adapter")
				}
				return
			}
			if len(provider.requests) != 1 {
				t.Fatalf("requests=%d", len(provider.requests))
			}
			got := provider.requests[0]
			if got.Operation != tc.operation || got.OriginalDeployment.ExternalExecutionID != tc.original || got.IdempotencyKey != request.IdempotencyKey || !got.DispatchedAt.Equal(now) || got.Target.Service != request.Target.Service || got.Target.Environment != request.Target.Environment || got.ArtifactDigest != request.Action.ArtifactDigest {
				t.Fatalf("request changed operation identity: %#v", got)
			}
		})
	}
}
