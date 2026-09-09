package runner

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"themisy/internal/identity"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

type ConnectionState bool

func (s ConnectionState) Connected() bool { return bool(s) }

type AtomicConnectionState struct{ connected atomic.Bool }

func (s *AtomicConnectionState) Connected() bool { return s != nil && s.connected.Load() }
func (s *AtomicConnectionState) Set(connected bool) {
	if s != nil {
		s.connected.Store(connected)
	}
}

type StaticContexts map[string]PinnedContext

func (s StaticContexts) ResolveContext(_ context.Context, runID, stepID string) (PinnedContext, error) {
	value, ok := s[runID+"\x00"+stepID]
	if !ok {
		return PinnedContext{}, fmt.Errorf("pinned context not found")
	}
	return value, nil
}

type ProofReferences map[string]struct{}

func (p ProofReferences) VerifyApprovals(_ context.Context, grant protocol.ActionGrant, _ identity.Subject) error {
	for _, reference := range grant.ApprovalProofs {
		if _, ok := p[reference]; !ok {
			return fmt.Errorf("untrusted approval proof")
		}
	}
	return nil
}

type Credential = credentials.Credential
type CredentialRequest = credentials.Request

type CredentialBroker interface {
	Acquire(context.Context, CredentialRequest) (Credential, error)
}

type AdapterRequest struct {
	GrantID        string
	RunID          string
	StepID         string
	Target         protocol.Target
	Action         protocol.Action
	IdempotencyKey string
	DispatchedAt   time.Time
}

type AdapterResult struct {
	ExternalExecutionID string
	CompletedAt         time.Time
}

// TypedAdapter intentionally exposes no command, URL, shell, or generic cloud operation.
type TypedAdapter interface {
	Execute(context.Context, AdapterRequest, Credential) (AdapterResult, error)
}

type TargetAuthorizer interface {
	Authorize(protocol.Target, protocol.Action) error
}

type Reconciler interface {
	Reconcile(context.Context, AdapterRequest, Credential) (AdapterResult, bool, error)
}

// SDKDispatcher is the only bridge from an authorized Runner request to the
// public typed Adapter SDK. It exposes neither workflow paths nor URLs.
type SDKDispatcher struct {
	Adapter adapter.DeployAdapter
	Now     func() time.Time
}

func (d *SDKDispatcher) CredentialProvider() string {
	if configured, ok := d.Adapter.(interface{ CredentialProvider() string }); ok {
		return configured.CredentialProvider()
	}
	return adapter.ProviderGitHubActions
}

func (d *SDKDispatcher) Authorize(target protocol.Target, action protocol.Action) error {
	if d.Adapter == nil {
		return fmt.Errorf("deploy adapter unavailable")
	}
	authorizer, ok := d.Adapter.(interface {
		Authorize(adapter.Target, string) error
	})
	if !ok {
		return fmt.Errorf("adapter does not expose a Runner target allowlist")
	}
	return authorizer.Authorize(adapter.Target{Service: target.Service, Environment: target.Environment}, action.ArtifactDigest)
}

func (d *SDKDispatcher) Execute(ctx context.Context, request AdapterRequest, credential Credential) (AdapterResult, error) {
	if d.Adapter == nil {
		return AdapterResult{}, fmt.Errorf("deploy adapter unavailable")
	}
	target := adapter.Target{Service: request.Target.Service, Environment: request.Target.Environment}
	var deployment adapter.Deployment
	var err error
	switch request.Action.Capability {
	case protocol.CapabilityDeploy:
		deployment, err = d.Adapter.Deploy(ctx, adapter.DeployRequest{Target: target, ArtifactDigest: request.Action.ArtifactDigest, IdempotencyKey: request.IdempotencyKey}, credential)
	case protocol.CapabilityRollback:
		deployment, err = d.Adapter.Rollback(ctx, adapter.RollbackRequest{Target: target, OriginalDeployment: adapter.Deployment{ExternalExecutionID: request.Action.ExternalExecutionID}, IdempotencyKey: request.IdempotencyKey}, credential)
	default:
		return AdapterResult{}, fmt.Errorf("unsupported typed action %q", request.Action.Capability)
	}
	if err != nil {
		return AdapterResult{}, err
	}
	completedAt := deployment.FinishedAt
	if completedAt.IsZero() {
		completedAt = d.now()
	}
	return AdapterResult{ExternalExecutionID: deployment.ExternalExecutionID, CompletedAt: completedAt}, nil
}

func (d *SDKDispatcher) Reconcile(ctx context.Context, request AdapterRequest, credential Credential) (AdapterResult, bool, error) {
	if d.Adapter == nil {
		return AdapterResult{}, false, fmt.Errorf("deploy adapter unavailable")
	}
	dispatchedAt := request.DispatchedAt
	if dispatchedAt.IsZero() {
		dispatchedAt = d.now().Add(-24 * time.Hour)
	}
	result, err := d.Adapter.Reconcile(ctx, adapter.ReconcileRequest{IdempotencyKey: request.IdempotencyKey, DispatchedAt: dispatchedAt, Target: adapter.Target{Service: request.Target.Service, Environment: request.Target.Environment}, ArtifactDigest: request.Action.ArtifactDigest}, credential)
	if err != nil {
		return AdapterResult{}, false, err
	}
	if result.Status == adapter.ReconcileNotFound || result.Status == adapter.ReconcilePending {
		return AdapterResult{}, false, nil
	}
	if result.Status == adapter.ReconcileFailed {
		return AdapterResult{}, false, fmt.Errorf("external execution failed during reconciliation")
	}
	return AdapterResult{ExternalExecutionID: result.Deployment.ExternalExecutionID, CompletedAt: result.Deployment.FinishedAt}, true, nil
}

func (d *SDKDispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}
