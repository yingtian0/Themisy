package application

import (
	"context"
	"fmt"
	"themisy/internal/domain"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func compensationFor(ctx context.Context, st store.DurableStore, run domain.ReleaseRun, step domain.ReleaseStep, planned domain.PlanStep, request GrantIssueRequest, now time.Time) (*protocol.CompensationBinding, error) {
	refuse := func(reason string) (*protocol.CompensationBinding, error) {
		return nil, fmt.Errorf("%w: %s; read-only reconciliation and operator review required", ErrGrantNotAuthorized, reason)
	}
	duration, err := time.ParseDuration(planned.RollbackDeadline)
	if err != nil || duration <= 0 || planned.RollbackTargetHash == "" || request.SourceGrantID == "" || step.Execution == nil || step.Execution.Status != string(store.ExecutionSucceeded) {
		return refuse("COMPENSATION_UNAVAILABLE")
	}
	source, err := st.GetGrantDispatch(ctx, request.SourceGrantID)
	if err != nil {
		return refuse("COMPENSATION_SOURCE_UNAVAILABLE")
	}
	g := source.Grant
	if source.Status != store.GrantDispatchSucceeded || source.Result.Status != protocol.ResultSucceeded || source.Result.GrantID != g.GrantID || source.Result.RunID != run.ID || source.Result.StepID != step.Service || g.TenantID != run.TenantID || g.RunID != run.ID || g.StepID != step.Service || g.RunnerGroup != planned.Scheduling.RunnerGroup || g.PlanHash != run.Plan.Hash || g.Target.Environment != string(run.Environment) || g.Target.Service != step.Service || g.Action.Capability != protocol.CapabilityDeploy || g.Action.ArtifactDigest != step.Change.DesiredVersion || g.Action.ExternalExecutionID != "" || source.Result.ExternalExecutionID == "" || source.Result.ExternalExecutionID != request.ExternalExecutionID || step.Execution.ExternalExecutionID != request.ExternalExecutionID || step.Execution.IdempotencyKey != g.IdempotencyKey {
		return refuse("COMPENSATION_SOURCE_MISMATCH")
	}
	// Start before dispatch/credential acquisition, never at response or recovery.
	until := g.IssuedAt.Add(duration)
	if !now.Before(until) {
		return refuse("COMPENSATION_EXPIRED")
	}
	hash, err := protocol.GrantHash(g)
	if err != nil {
		return refuse("COMPENSATION_SOURCE_UNAVAILABLE")
	}
	return &protocol.CompensationBinding{SourceGrantID: g.GrantID, SourceNonce: g.Nonce, SourceRequestHash: hash, SourceIssuedAt: g.IssuedAt.Format(time.RFC3339Nano), ValidUntil: until.Format(time.RFC3339Nano), TargetHash: planned.RollbackTargetHash}, nil
}
