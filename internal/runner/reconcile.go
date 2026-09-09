package runner

import (
	"context"
	"fmt"

	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

// Reconcile resolves already-reserved writes. It may run while disconnected,
// but it never starts a new adapter write.
func (r *Runner) Reconcile(ctx context.Context, reconciler Reconciler, limit int) error {
	if r.Journal == nil || reconciler == nil {
		return fmt.Errorf("reconciliation unavailable")
	}
	if r.TenantID == "" || r.RunnerGroup == "" {
		return fmt.Errorf("runner identity unavailable")
	}
	records, err := r.Journal.PendingRunnerActions(ctx, r.TenantID, r.RunnerGroup, limit)
	if err != nil {
		return err
	}
	for _, record := range records {
		if r.Credentials == nil {
			return fmt.Errorf("reconciliation credential unavailable")
		}
		provider := "typed-adapter"
		if configured, ok := reconciler.(interface{ CredentialProvider() string }); ok {
			provider = configured.CredentialProvider()
		}
		purpose := credentials.PurposeDeploy
		if record.Action.Capability == protocol.CapabilityRollback {
			purpose = credentials.PurposeRollback
		}
		credential, err := r.Credentials.Acquire(ctx, CredentialRequest{Provider: provider, TenantID: record.TenantID, Service: record.Target.Service, Environment: record.Target.Environment, Purpose: purpose, GrantID: record.GrantID})
		if err != nil {
			continue
		}
		request := AdapterRequest{GrantID: record.GrantID, RunID: record.RunID, StepID: record.StepID, Target: record.Target, Action: record.Action, IdempotencyKey: record.IdempotencyKey, DispatchedAt: record.CreatedAt}
		result, found, err := reconciler.Reconcile(ctx, request, credential)
		if err != nil || !found {
			continue
		}
		now := r.now()
		record.Status = store.RunnerActionSucceeded
		record.Result = protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.GrantID, RunID: record.RunID, StepID: record.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: result.ExternalExecutionID, CompletedAt: result.CompletedAt}
		record.UpdatedAt = now
		if err := r.Journal.CompleteRunnerAction(ctx, record, record.StateVersion, journalAudit(auditID(record.GrantID, "reconcile", record.StateVersion+1), record.GrantID, "runner.action.reconcile", "SUCCEEDED", now, nil)); err != nil {
			return err
		}
	}
	return nil
}
