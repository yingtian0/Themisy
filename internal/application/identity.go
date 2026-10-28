package application

import (
	"context"
	"fmt"
	"slices"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/pkg/protocol"
	"time"
)

// IdentityAuthority is configured only for OIDC mode. Development compatibility
// does not weaken the production chain: it uses a separate explicit mode.
type IdentityAuthority struct {
	Verifier *identity.AttestationVerifier
	Signer   grant.Signer
	Audit    interface {
		AppendAuditContext(context.Context, domain.AuditEvent) error
	}
	KeyID string
}

func (a *IdentityAuthority) Prepare(ctx context.Context, intent domain.ReleaseIntent) (domain.ReleaseIntent, error) {
	actor, ok := identity.RequestActorFromContext(ctx)
	if !ok || a == nil || a.Verifier == nil || a.Verifier.OIDC == nil {
		return intent, identity.ErrInvalidIdentityProof
	}
	if err := identity.BindIntentActor(ctx, &intent); err != nil {
		return intent, err
	}
	if actor.TenantID != a.Verifier.TenantID {
		return intent, identity.ErrInvalidIdentityProof
	}
	proof, err := identity.Attest(ctx, a.Verifier.OIDC, a.Signer, a.KeyID, identity.Attestation{Issuer: a.Verifier.Issuer, TenantID: actor.TenantID, Audience: a.Verifier.Audience, SubjectType: actor.SubjectType, RequestID: intent.RequestID, AgentID: intent.Agent.ID, DelegationRef: intent.DelegationRef, Roles: actor.Roles}, actor.ProofToken)
	if err != nil {
		return intent, err
	}
	intent.UserIdentityProof = proof
	return intent, nil
}
func (a *IdentityAuthority) Authorize(ctx context.Context, run domain.ReleaseRun, step domain.ReleaseStep, planned domain.PlanStep, capability protocol.Capability) (time.Time, error) {
	if a == nil || a.Audit == nil {
		return time.Time{}, identity.ErrInvalidIdentityProof
	}
	until, err := a.authorize(ctx, run, step, planned, capability)
	result, reason := "VERIFIED", "IDENTITY_AUTHORITY_VERIFIED"
	if err != nil {
		result, reason = "REJECTED", "IDENTITY_AUTHORITY_UNAVAILABLE"
	}
	now := time.Now().UTC()
	if a.Verifier != nil && a.Verifier.Now != nil {
		now = a.Verifier.Now().UTC()
	}
	event := domain.AuditEvent{ID: newID("identity"), CorrelationID: run.ID, ActorType: "system", ActorID: "identity-authority", Action: "identity.authorize", ResourceType: "identity_decision", ResourceID: run.ID, Result: result, Timestamp: now, Details: map[string]any{"tenant_id": run.TenantID, "issuer": run.SubjectIssuer, "subject": run.RequestedBy, "delegation_ref": run.DelegationRef, "capability": capability, "step_id": step.Service, "reason_code": reason}}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.Audit.AppendAuditContext(bounded, event) != nil || bounded.Err() != nil {
		return time.Time{}, identity.ErrInvalidIdentityProof
	}
	now = time.Now().UTC()
	if a.Verifier != nil && a.Verifier.Now != nil {
		now = a.Verifier.Now().UTC()
	}
	if err != nil || !until.After(now) {
		return time.Time{}, identity.ErrInvalidIdentityProof
	}
	return until, nil
}
func (a *IdentityAuthority) authorize(ctx context.Context, run domain.ReleaseRun, step domain.ReleaseStep, planned domain.PlanStep, capability protocol.Capability) (time.Time, error) {
	if a == nil || a.Verifier == nil {
		return time.Time{}, identity.ErrInvalidIdentityProof
	}
	receipt, err := a.Verifier.VerifyAttestation(ctx, run.UserIdentityProof)
	if err != nil || receipt.TenantID != run.TenantID || receipt.SubjectID != run.RequestedBy || receipt.SubjectIssuer != run.SubjectIssuer || receipt.SubjectType != run.SubjectType || receipt.RequestID != run.RequestID || receipt.AgentID != run.Agent.ID || receipt.DelegationRef != run.DelegationRef {
		return time.Time{}, identity.ErrInvalidIdentityProof
	}
	if run.Plan.Hash != "" {
		p := run.Plan.Requester
		if p == nil || p.TenantID != run.TenantID || p.Subject != run.RequestedBy || p.Issuer != run.SubjectIssuer || p.SubjectType != run.SubjectType || p.AgentID != run.Agent.ID || p.DelegationRef != run.DelegationRef {
			return time.Time{}, identity.ErrInvalidIdentityProof
		}
	}
	member, err := a.Verifier.Directory.Member(ctx, receipt.SubjectIssuer, receipt.SubjectID)
	if err != nil || !slices.Contains(receipt.Roles, "deployer") || !slices.Contains(member.Roles, "deployer") {
		return time.Time{}, identity.ErrDelegationDenied
	}
	subject, err := a.Verifier.Verify(ctx, run.UserIdentityProof)
	if err != nil {
		return time.Time{}, err
	}
	delegation, err := (&identity.DelegationVerifier{TenantID: run.TenantID, TenantClaim: "tenant_id", Resolver: a.Verifier.Directory, Now: a.Verifier.Now}).Verify(ctx, identity.DelegationRequest{Reference: run.DelegationRef, Subject: subject, AgentID: run.Agent.ID, Capability: capability, Service: step.Service, Environment: string(run.Environment), Risk: effectiveRisk(step.Change.Risk, planned.Scheduling.RiskTier)})
	if err != nil {
		return time.Time{}, err
	}
	if step.Approval != nil && step.Approval.Status == domain.ApprovalApproved {
		approval := step.Approval
		if approval.DecidedIssuer == "" || approval.DecidedBy == "" || (approval.DecidedIssuer == receipt.SubjectIssuer && approval.DecidedBy == receipt.SubjectID) {
			return time.Time{}, fmt.Errorf("verified independent approver required")
		}
		approver, err := a.Verifier.Directory.Member(ctx, approval.DecidedIssuer, approval.DecidedBy)
		if err != nil || !containsAll(approver.Roles, approval.RequiredRoles) {
			return time.Time{}, fmt.Errorf("approver authority unavailable")
		}
	}
	expires := receipt.ExpiresAt
	if delegation.ExpiresAt.Before(expires) {
		expires = delegation.ExpiresAt
	}
	bundle, err := a.Verifier.Directory.Read(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if bundle.ExpiresAt.Before(expires) {
		expires = bundle.ExpiresAt
	}
	return expires, nil
}
func effectiveRisk(requested, pinned string) string { return policy.EffectiveRisk(requested, pinned) }
