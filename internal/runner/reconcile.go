package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

// Reconcile resolves already-reserved writes. It may run while disconnected,
// but it never starts a new adapter write.
func (r *Runner) Reconcile(ctx context.Context, reconciler Reconciler, limit int) (retErr error) {
	if r.Journal == nil || reconciler == nil {
		return fmt.Errorf("reconciliation unavailable")
	}
	if r.TenantID == "" || r.RunnerGroup == "" {
		return fmt.Errorf("runner identity unavailable")
	}
	defer func() { retErr = errors.Join(retErr, r.reportRecovery(ctx, limit)) }()
	var records []store.RunnerActionRecord
	var err error
	if journal, ok := r.Journal.(store.RecoveryJournal); ok {
		records, err = journal.ClaimRunnerActions(ctx, r.TenantID, r.RunnerGroup, r.RunnerID, r.now(), r.now().Add(time.Minute), limit)
	} else {
		records, err = r.Journal.PendingRunnerActions(ctx, r.TenantID, r.RunnerGroup, limit)
	}
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
			now := r.now()
			record.Status = store.RunnerActionUnknown
			record.Result = protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.GrantID, RunID: record.RunID, StepID: record.StepID, Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_UNRESOLVED", CompletedAt: now}
			record.UpdatedAt = now
			if err := r.Journal.CompleteRunnerAction(ctx, record, record.StateVersion, journalAudit(auditID(record.GrantID, "unresolved", record.StateVersion+1), record.GrantID, "runner.reconcile.unresolved", "UNKNOWN", now, nil)); err != nil {
				return err
			}
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

func (r *Runner) reportRecovery(ctx context.Context, limit int) error {
	if r.Reporter == nil {
		return nil
	}
	journal, ok := r.Journal.(interface {
		UnreportedRunnerActions(context.Context, string, string, int) ([]store.RunnerActionRecord, error)
		MarkRunnerReported(context.Context, store.RunnerActionRecord) error
	})
	if !ok {
		return nil
	}
	records, err := journal.UnreportedRunnerActions(ctx, r.TenantID, r.RunnerGroup, limit)
	if err != nil {
		return err
	}
	var reportErr error
	for _, record := range records {
		if err := r.Reporter.ReportRecovery(ctx, record); err == nil {
			reportErr = errors.Join(reportErr, journal.MarkRunnerReported(ctx, record))
		} else {
			reportErr = errors.Join(reportErr, err)
		}
	}
	return reportErr
}
