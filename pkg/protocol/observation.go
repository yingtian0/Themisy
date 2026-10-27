package protocol

import (
	"fmt"
	"themisy/pkg/adapter"
	"time"
)

// ValidateObservationResult prevents a signed receipt being applied to another
// source, target, operation or observation window.
func ValidateObservationResult(g ActionGrant, r Result) error {
	if g.Action.Capability != CapabilityVerify || r.GrantID != g.GrantID || r.RunID != g.RunID || r.StepID != g.StepID || r.ExternalExecutionID != g.Action.ExternalExecutionID || r.Evidence == nil || r.Health == nil || r.Runtime == nil {
		return fmt.Errorf("observation binding unavailable")
	}
	from, first := time.Parse(time.RFC3339Nano, g.Action.VerificationFrom)
	to, second := time.Parse(time.RFC3339Nano, g.Action.VerificationTo)
	e := r.Evidence
	v := r.Runtime
	aggregate, aggregateErr := adapter.CombinedEvidence(*v, *r.Health)
	if aggregateErr != nil || aggregate.EvidenceHash != r.EvidenceHash {
		return fmt.Errorf("combined evidence mismatch")
	}
	if v.Status == adapter.VerificationPass {
		setHash, setErr := adapter.ContainerSetHash(v.ContainerDigests)
		if setErr != nil || len(v.ContainerDigests) == 0 || setHash != v.ContainerSetHash || v.Summary.DesiredCount <= 0 || v.Summary.RunningCount != v.Summary.DesiredCount || v.Summary.PendingCount != 0 || v.Summary.TaskCount != int(v.Summary.DesiredCount) || v.Summary.RolloutState != "COMPLETED" {
			return fmt.Errorf("runtime success lacks matching observed counts and containers")
		}
	}
	op := adapter.ReconcileDeploy
	if g.Action.SourceCapability == CapabilityRollback {
		op = adapter.ReconcileRollback
	}
	request := adapter.ReconcileRequest{Operation: op, Target: adapter.Target{Service: g.Target.Service, Environment: g.Target.Environment}, ArtifactDigest: g.Action.ArtifactDigest, IdempotencyKey: g.Action.SourceIdempotencyKey, OriginalDeployment: adapter.Deployment{ExternalExecutionID: g.Action.SourceExternalExecutionID}}
	expected, err := adapter.RuntimeRequestHash(request)
	rh, hashErr := adapter.RuntimeObservationHash(*v)
	eh, evidenceErr := adapter.EvidenceHash(*e)
	if first != nil || second != nil || err != nil || hashErr != nil || evidenceErr != nil || rh != v.Hash || eh != e.EvidenceHash || eh != r.EvidenceHash || adapter.ValidateEvidence(*e) != nil || v.SchemaVersion != "themisy.ecs-runtime/v1alpha1" || v.AdapterVersion == "" || !v.Status.Valid() || v.Target != request.Target || v.Operation != op || v.RequestHash != expected || !e.Window.From.Equal(from) || !e.Window.To.Equal(to) || e.ObservedAt.Before(to) || v.ObservedAt.Before(to) || e.ObservedAt.After(r.CompletedAt) || v.ObservedAt.After(r.CompletedAt) {
		return fmt.Errorf("observation scope, hash or chronology mismatch")
	}
	return nil
}
