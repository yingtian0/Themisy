package application

import (
	"context"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

// ApprovedManifest is invoked by the independent approval/provisioning path.
// It reads durable approved Plan state, never an ActionGrant payload. Its signer
// should be controlled by the approval authority, not the dispatch service.
func ApprovedManifest(ctx context.Context, st store.DurableStore, signer grant.Signer, issuer string, request GrantIssueRequest, revision int64, now time.Time) (protocol.PlanManifest, error) {
	run, err := st.GetRun(request.RunID)
	if err != nil {
		return protocol.PlanManifest{}, err
	}
	step, planned, err := authorizedStep(*run, request.StepID, request.Capability, now)
	if err != nil {
		return protocol.PlanManifest{}, err
	}
	p := protocol.PlanManifest{ProtocolVersion: protocol.PlanManifestVersion, Issuer: issuer, TenantID: run.TenantID, RunnerGroup: planned.Scheduling.RunnerGroup, RunID: run.ID, StepID: step.Service, Revision: revision, PlanHash: run.Plan.Hash, ContractHash: planned.ContractHash, ProfileHash: planned.ProfileHash, PolicyHash: run.Plan.PolicyHash, EvidenceHash: run.Plan.EvidenceHash, Target: protocol.Target{Service: step.Service, Environment: string(run.Environment)}, Action: protocol.Action{Capability: request.Capability, ArtifactDigest: step.Change.DesiredVersion, ExternalExecutionID: request.ExternalExecutionID}, IssuedAt: now, ExpiresAt: run.Plan.ExpiresAt}
	if step.Approval != nil {
		p.ApprovalRequired = true
		p.ApprovalProofs = []string{step.Approval.ID}
		if step.Approval.ExpiresAt.Before(p.ExpiresAt) {
			p.ExpiresAt = step.Approval.ExpiresAt
		}
	}
	return grant.SignPlanManifest(ctx, signer, p)
}
