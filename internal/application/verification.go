package application

import (
	"context"
	"errors"
	"fmt"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"themisy/pkg/protocol"
	"time"
)

// VerifyDeployment requests observations of an already completed operation.
// It grants no new write, even when the original Plan/identity has expired.
func (g *Grants) VerifyDeployment(ctx context.Context, d executor.Deployment, interval time.Duration) (executor.VerificationResult, error) {
	unavailable := errors.New("verified deployment observation unavailable")
	if g == nil || g.Store == nil || g.Signer == nil || ctx.Err() != nil || d.GrantID == "" {
		return executor.VerificationResult{}, unavailable
	}
	source, err := g.Store.GetGrantDispatch(ctx, d.GrantID)
	if err != nil || source.Status != store.GrantDispatchSucceeded || source.Result.Status != protocol.ResultSucceeded || source.Grant.RunID != d.RunID || source.Grant.Target.Service != d.Service || source.Grant.Target.Environment != d.Environment || source.Result.ExternalExecutionID != d.ExternalID || source.Grant.Action.ArtifactDigest != d.DesiredVersion || !source.Result.CompletedAt.Equal(d.FinishedAt) || (source.Grant.Action.Capability != protocol.CapabilityDeploy && source.Grant.Action.Capability != protocol.CapabilityRollback) {
		return executor.VerificationResult{}, unavailable
	}
	run, err := g.Store.GetRun(d.RunID)
	if err != nil || run.TenantID != source.Grant.TenantID || run.Plan.Hash != source.Grant.PlanHash {
		return executor.VerificationResult{}, unavailable
	}
	var duration time.Duration
	matches := 0
	for _, phase := range run.Plan.Phases {
		for _, planned := range phase.Steps {
			if planned.Service == d.Service {
				matches++
				duration, err = time.ParseDuration(planned.ObservationWindow)
			}
		}
	}
	if matches != 1 || err != nil || duration <= 0 || duration > 24*time.Hour || source.Result.CompletedAt.Before(source.Grant.IssuedAt) || source.Result.CompletedAt.IsZero() {
		return executor.VerificationResult{}, unavailable
	}
	now := g.now()
	from := source.Result.CompletedAt.UTC()
	to := from.Add(duration)
	if to.After(now) {
		return executor.VerificationResult{}, unavailable
	}
	sourceHash, err := protocol.GrantHash(source.Grant)
	if err != nil {
		return executor.VerificationResult{}, unavailable
	}
	request := source.Grant
	request.GrantID = newID("observation")
	request.Nonce = newID("observation_nonce")
	request.Signature = protocol.Signature{}
	request.Action = protocol.Action{Capability: protocol.CapabilityVerify, ArtifactDigest: source.Grant.Action.ArtifactDigest, ExternalExecutionID: d.ExternalID, SourceGrantID: source.Grant.GrantID, SourceNonce: source.Grant.Nonce, SourceExternalExecutionID: source.Grant.Action.ExternalExecutionID, SourceRequestHash: sourceHash, SourceIdempotencyKey: source.Grant.IdempotencyKey, SourceCapability: source.Grant.Action.Capability, VerificationFrom: from.Format(time.RFC3339Nano), VerificationTo: to.Format(time.RFC3339Nano)}
	request.IdempotencyKey = "observation/" + sourceHash + "/" + to.Format(time.RFC3339Nano)
	request.IssuedAt = now
	request.ExpiresAt = now.Add(g.TTL)
	request, err = grant.SignActionGrant(ctx, g.Signer, request)
	if err != nil {
		return executor.VerificationResult{}, unavailable
	}
	audit := domain.AuditEvent{ID: newID("audit"), CorrelationID: d.RunID, ActorType: "system", ActorID: "observation-requester", Action: "verification.request", ResourceType: "runtime_observation", ResourceID: request.GrantID, Result: "REQUESTED", Timestamp: now, Details: map[string]any{"source_grant_id": d.GrantID, "window_from": from, "window_to": to}}
	outbox := domain.OutboxEvent{ID: "observation/" + request.GrantID, AggregateType: "action_grant", AggregateID: request.GrantID, EventType: "observation.requested", CreatedAt: now, AvailableAt: now}
	record, _, err := g.Store.CreateGrantDispatch(ctx, store.GrantDispatchRecord{Grant: request, CreatedAt: now, UpdatedAt: now}, audit, outbox)
	if err != nil || record.Grant.Action != request.Action || record.Grant.RunID != d.RunID || record.Grant.Target != request.Target {
		return executor.VerificationResult{}, unavailable
	}
	result, err := g.WaitResult(ctx, record.Grant.GrantID, interval)
	if err != nil || result.Status != protocol.ResultSucceeded || result.Evidence == nil || result.Runtime == nil || result.CompletedAt.After(g.now().Add(30*time.Second)) {
		return executor.VerificationResult{}, unavailable
	}
	if protocol.ValidateObservationResult(record.Grant, result) != nil {
		return executor.VerificationResult{}, unavailable
	}
	e := *result.Evidence
	runtime := *result.Runtime
	hash, err := adapter.EvidenceHash(e)
	runtimeHash, runtimeErr := adapter.RuntimeObservationHash(runtime)
	if err != nil || runtimeErr != nil || adapter.ValidateEvidence(e) != nil || hash != e.EvidenceHash || hash != result.EvidenceHash || runtimeHash != runtime.Hash || runtime.Target != (adapter.Target{Service: d.Service, Environment: d.Environment}) || !e.Window.From.Equal(from) || !e.Window.To.Equal(to) || e.ObservedAt.Before(to) || runtime.ObservedAt.Before(to) || e.ObservedAt.After(result.CompletedAt) || runtime.ObservedAt.After(result.CompletedAt) || !runtime.Status.Valid() {
		return executor.VerificationResult{}, unavailable
	}
	status := e.Status
	reason := e.ReasonCode
	if !status.Valid() {
		return executor.VerificationResult{}, fmt.Errorf("invalid observation status")
	}
	return executor.VerificationResult{ObservationGrantID: record.Grant.GrantID, SourceGrantID: d.GrantID, Status: status, Healthy: status == adapter.VerificationPass, Reason: reason, ObservedValue: e.ObservedValue, Threshold: e.Threshold, Evidence: e, Runtime: &runtime}, nil
}
