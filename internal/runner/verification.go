package runner

import (
	"context"
	"errors"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
	"time"
)

type RuntimeReader interface {
	ReadRuntime(context.Context, adapter.ReconcileRequest, credentials.Credential) (adapter.RuntimeObservation, error)
}

// ObservationVerifier contains only read interfaces. It cannot deploy/rollback.
type ObservationVerifier struct {
	Runtime       RuntimeReader
	RuntimeBroker credentials.Broker
	Health        adapter.VerificationAdapter
	HealthBroker  credentials.Broker
}

func (r *Runner) observe(ctx context.Context, g protocol.ActionGrant) (protocol.Result, error) {
	reject := func(reason Reason) (protocol.Result, error) {
		return r.reject(g, reason, errors.New("source-bound observation unavailable"))
	}
	if ctx.Err() != nil || r.Journal == nil || r.Observations == nil || r.Observations.Runtime == nil || r.Observations.RuntimeBroker == nil || r.Observations.Health == nil || r.Observations.HealthBroker == nil || !r.Capabilities.Allows(protocol.CapabilityVerify) {
		return reject(ReasonCapability)
	}
	if r.Connectivity == nil || !r.Connectivity.Connected() {
		return reject(ReasonDisconnected)
	}
	source, err := r.Journal.GetRunnerAction(ctx, g.TenantID, g.RunnerGroup, g.Action.SourceNonce)
	if err != nil || source.Status != store.RunnerActionSucceeded || source.Result.Status != protocol.ResultSucceeded || source.Result.GrantID != source.GrantID || source.Result.RunID != source.RunID || source.Result.StepID != source.StepID || source.GrantID != g.Action.SourceGrantID || source.RequestHash != g.Action.SourceRequestHash || source.IdempotencyKey != g.Action.SourceIdempotencyKey || source.RunID != g.RunID || source.StepID != g.StepID || source.PlanHash != g.PlanHash || source.Target != g.Target || source.Action.ExternalExecutionID != g.Action.SourceExternalExecutionID || source.Action.Capability != g.Action.SourceCapability || source.Action.ArtifactDigest != g.Action.ArtifactDigest || source.Result.ExternalExecutionID != g.Action.ExternalExecutionID {
		return reject(ReasonHashMismatch)
	}
	from, err := time.Parse(time.RFC3339Nano, g.Action.VerificationFrom)
	if err != nil {
		return reject(ReasonHashMismatch)
	}
	to, err := time.Parse(time.RFC3339Nano, g.Action.VerificationTo)
	if err != nil || !source.Result.CompletedAt.Equal(from) || to.After(r.now()) {
		return reject(ReasonHashMismatch)
	}
	hash, err := protocol.GrantHash(g)
	if err != nil {
		return reject(ReasonHashMismatch)
	}
	now := r.now()
	record := store.RunnerActionRecord{GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, Nonce: g.Nonce, IdempotencyKey: g.IdempotencyKey, RequestHash: hash, Target: g.Target, Action: g.Action, CreatedAt: now, UpdatedAt: now, RunnerID: r.RunnerID, PlanHash: g.PlanHash}
	record, created, err := r.Journal.ReserveRunnerAction(ctx, record, journalAudit(auditID(g.GrantID, "observe", 1), g.RunID, "runner.observation.request", "AUTHORIZED_READ", now, map[string]any{"source_grant_id": source.GrantID}))
	if err != nil {
		return reject(ReasonJournalUnavailable)
	}
	if !created && record.RequestHash != hash {
		return reject(ReasonReplay)
	}
	if !created && record.Status == store.RunnerActionSucceeded {
		return record.Result, nil
	}
	request := adapter.ReconcileRequest{Operation: adapter.ReconcileDeploy, IdempotencyKey: source.IdempotencyKey, DispatchedAt: source.CreatedAt, Target: adapter.Target{Service: g.Target.Service, Environment: g.Target.Environment}, ArtifactDigest: source.Action.ArtifactDigest}
	if source.Action.Capability == protocol.CapabilityRollback {
		request.Operation = adapter.ReconcileRollback
		request.OriginalDeployment.ExternalExecutionID = source.Action.ExternalExecutionID
	}
	credentialRequest := credentials.Request{Provider: credentials.ProviderAWS, TenantID: g.TenantID, RunID: g.RunID, GrantID: g.GrantID, Service: g.Target.Service, Environment: g.Target.Environment, Purpose: credentials.PurposeVerify}
	credential, err := r.Observations.RuntimeBroker.Acquire(ctx, credentialRequest)
	if err != nil {
		return reject(ReasonCredential)
	}
	runtime, err := r.Observations.Runtime.ReadRuntime(ctx, request, credential)
	if err != nil {
		return reject(ReasonExternalUnknown)
	}
	credentialRequest.Provider = adapter.ProviderDatadog
	credential, err = r.Observations.HealthBroker.Acquire(ctx, credentialRequest)
	if err != nil {
		return reject(ReasonCredential)
	}
	health, err := r.Observations.Health.Verify(ctx, adapter.VerificationRequest{Target: request.Target, Deployment: adapter.Deployment{ExternalExecutionID: source.Result.ExternalExecutionID}, Window: adapter.ObservationWindow{From: from, To: to}}, credential)
	if err != nil || ctx.Err() != nil {
		return reject(ReasonExternalUnknown)
	}
	aggregate, err := adapter.CombinedEvidence(runtime, health)
	if err != nil {
		return reject(ReasonExternalUnknown)
	}
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: source.Result.ExternalExecutionID, CompletedAt: r.now(), Evidence: &aggregate, Health: &health, EvidenceHash: aggregate.EvidenceHash, Runtime: &runtime}
	if protocol.ValidateObservationResult(g, result) != nil {
		return reject(ReasonHashMismatch)
	}
	// The canonical report validator also checks evidence completeness, hashes and chronology.
	if _, err := protocol.CanonicalResultReportPayload(protocol.ResultReport{Version: protocol.ResultReportVersion, RunnerID: r.RunnerID, TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, RequestHash: hash, Result: result, Signature: protocol.Signature{Algorithm: "Ed25519", KeyID: "validation"}}); err != nil {
		return reject(ReasonExternalUnknown)
	}
	if err := r.Grants.Verify(ctx, g); err != nil {
		return reject(ReasonIdentity)
	}
	record.Result = result
	record.Status = store.RunnerActionSucceeded
	record.UpdatedAt = r.now()
	if err := r.Journal.CompleteRunnerAction(ctx, record, record.StateVersion, journalAudit(auditID(g.GrantID, "observation", record.StateVersion+1), g.RunID, "runner.observation.complete", "OBSERVED", record.UpdatedAt, map[string]any{"source_grant_id": source.GrantID, "runtime": runtime, "evidence": health})); err != nil {
		return reject(ReasonJournalUnavailable)
	}
	return result, nil
}
