package runner

import (
	"context"
	"errors"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func (r *Runner) verifyCompensation(ctx context.Context, g protocol.ActionGrant) error {
	c := g.Compensation
	if c == nil || r.Journal == nil {
		return errors.New("COMPENSATION_UNAVAILABLE")
	}
	until, err := time.Parse(time.RFC3339Nano, c.ValidUntil)
	if err != nil || !r.now().Before(until) {
		return errors.New("COMPENSATION_EXPIRED")
	}
	source, err := r.Journal.GetRunnerAction(ctx, g.TenantID, g.RunnerGroup, c.SourceNonce)
	if err != nil || source.Status != store.RunnerActionSucceeded || source.Result.Status != protocol.ResultSucceeded || source.GrantID != c.SourceGrantID || source.Result.GrantID != c.SourceGrantID || source.RequestHash != c.SourceRequestHash || source.RunID != g.RunID || source.StepID != g.StepID || source.PlanHash != g.PlanHash || source.Target != g.Target || source.Action.Capability != protocol.CapabilityDeploy || source.Action.ArtifactDigest != g.Action.ArtifactDigest || source.Result.ExternalExecutionID == "" || source.Result.ExternalExecutionID != g.Action.ExternalExecutionID {
		return errors.New("COMPENSATION_SOURCE_MISMATCH")
	}
	// The source journal was created after Grant issue and before any Write.
	from, err := time.Parse(time.RFC3339Nano, c.SourceIssuedAt)
	if err != nil || from.After(source.CreatedAt) {
		return errors.New("COMPENSATION_SOURCE_TIME_MISMATCH")
	}
	targets, ok := r.Adapter.(interface {
		CompensationTargetHash(protocol.Target) (string, error)
	})
	if !ok {
		return errors.New("COMPENSATION_TARGET_UNAVAILABLE")
	}
	hash, err := targets.CompensationTargetHash(g.Target)
	if err != nil || hash != c.TargetHash {
		return errors.New("COMPENSATION_TARGET_MISMATCH")
	}
	return nil
}
