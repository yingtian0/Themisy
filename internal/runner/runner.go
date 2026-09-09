package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

type Reason string

const (
	ReasonDisconnected       Reason = "CONTROL_PLANE_DISCONNECTED"
	ReasonTenant             Reason = "WRONG_TENANT"
	ReasonTarget             Reason = "TARGET_NOT_ALLOWED"
	ReasonReplay             Reason = "NONCE_REPLAY"
	ReasonIdentity           Reason = "IDENTITY_INVALID"
	ReasonDelegation         Reason = "DELEGATION_SCOPE_DENIED"
	ReasonHashMismatch       Reason = "HASH_MISMATCH"
	ReasonApproval           Reason = "APPROVAL_PROOF_MISSING"
	ReasonCapability         Reason = "UNSUPPORTED_CAPABILITY"
	ReasonPolicyDeny         Reason = "RUNNER_POLICY_DENY"
	ReasonPolicyMismatch     Reason = "POLICY_MISMATCH"
	ReasonJournalUnavailable Reason = "AUDIT_JOURNAL_UNAVAILABLE"
	ReasonCredential         Reason = "CREDENTIAL_UNAVAILABLE"
	ReasonExternalUnknown    Reason = "EXTERNAL_STATE_UNKNOWN"
	ReasonDraining           Reason = "RUNNER_DRAINING"
	ReasonFrozen             Reason = "RUNNER_FROZEN"
	ReasonCapacity           Reason = "RUNNER_AT_CAPACITY"
)

type Rejection struct {
	Reason Reason
	Err    error
}

func (e *Rejection) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}
func (e *Rejection) Unwrap() error { return e.Err }

type GrantVerifier interface {
	Verify(context.Context, protocol.ActionGrant) error
}
type SubjectVerifier interface {
	Verify(context.Context, string) (identity.Subject, error)
}
type DelegationAuthorizer interface {
	Verify(context.Context, identity.DelegationRequest) (identity.Delegation, error)
}
type PolicyEvaluator interface {
	EvaluateContext(context.Context, policy.Input) (policy.Evaluation, error)
}
type Connectivity interface{ Connected() bool }

type PinnedContext struct {
	PlanHash, ContractHash, ProfileHash, PolicyHash, EvidenceHash string
	ApprovalRequired                                              bool
	Target                                                        protocol.Target
	Action                                                        protocol.Action
}
type ContextResolver interface {
	ResolveContext(context.Context, string, string) (PinnedContext, error)
}
type ApprovalVerifier interface {
	VerifyApprovals(context.Context, protocol.ActionGrant, identity.Subject) error
}

type Runner struct {
	TenantID     string
	RunnerGroup  string
	Grants       GrantVerifier
	Subjects     SubjectVerifier
	Delegations  DelegationAuthorizer
	Contexts     ContextResolver
	Approvals    ApprovalVerifier
	Capabilities CapabilitySet
	Policy       PolicyEvaluator
	Journal      store.RunnerJournal
	Credentials  CredentialBroker
	Adapter      TypedAdapter
	Connectivity Connectivity
	Admission    AdmissionGate
	Now          func() time.Time
}

func (r *Runner) Execute(ctx context.Context, actionGrant protocol.ActionGrant) (protocol.Result, error) {
	// The verifier fixes protocol -> signature/issuer/audience -> expiry ordering.
	if r.Grants == nil {
		return r.reject(actionGrant, ReasonIdentity, errors.New("grant verifier unavailable"))
	}
	if err := r.Grants.Verify(ctx, actionGrant); err != nil {
		return r.reject(actionGrant, reasonForGrant(err), err)
	}
	if r.TenantID == "" || actionGrant.TenantID != r.TenantID {
		return r.reject(actionGrant, ReasonTenant, nil)
	}
	if r.RunnerGroup == "" || actionGrant.RunnerGroup != r.RunnerGroup {
		return r.reject(actionGrant, Reason("WRONG_RUNNER_AUDIENCE"), nil)
	}
	authorizer, ok := r.Adapter.(TargetAuthorizer)
	if !ok {
		return r.reject(actionGrant, ReasonTarget, errors.New("target authorizer unavailable"))
	}
	if err := authorizer.Authorize(actionGrant.Target, actionGrant.Action); err != nil {
		return r.reject(actionGrant, ReasonTarget, err)
	}
	requestHash, err := protocol.GrantHash(actionGrant)
	if err != nil {
		return r.reject(actionGrant, ReasonHashMismatch, err)
	}
	if r.Journal == nil {
		return r.reject(actionGrant, ReasonJournalUnavailable, nil)
	}
	existing, err := r.Journal.GetRunnerAction(ctx, actionGrant.TenantID, actionGrant.RunnerGroup, actionGrant.Nonce)
	if err == nil {
		return replayResult(actionGrant, existing, requestHash)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return r.reject(actionGrant, ReasonJournalUnavailable, err)
	}
	if r.Connectivity == nil || !r.Connectivity.Connected() {
		return r.reject(actionGrant, ReasonDisconnected, nil)
	}
	if r.Subjects == nil || actionGrant.UserIdentityProof == "" {
		return r.reject(actionGrant, ReasonIdentity, nil)
	}
	subject, err := r.Subjects.Verify(ctx, actionGrant.UserIdentityProof)
	if err != nil || subject.ID != actionGrant.UserSubject {
		return r.reject(actionGrant, ReasonIdentity, err)
	}
	if r.Delegations == nil {
		return r.reject(actionGrant, ReasonDelegation, nil)
	}
	delegation, err := r.Delegations.Verify(ctx, identity.DelegationRequest{Reference: actionGrant.DelegationRef, Subject: subject, AgentID: actionGrant.AgentID, Capability: actionGrant.Action.Capability, Service: actionGrant.Target.Service, Environment: actionGrant.Target.Environment, Risk: actionGrant.Risk})
	if err != nil {
		return r.reject(actionGrant, ReasonDelegation, err)
	}
	if r.Contexts == nil {
		return r.reject(actionGrant, ReasonHashMismatch, nil)
	}
	pinned, err := r.Contexts.ResolveContext(ctx, actionGrant.RunID, actionGrant.StepID)
	if err != nil || !matchingHashes(actionGrant, pinned) {
		return r.reject(actionGrant, ReasonHashMismatch, err)
	}
	if pinned.ApprovalRequired && len(actionGrant.ApprovalProofs) == 0 {
		return r.reject(actionGrant, ReasonApproval, nil)
	}
	if len(actionGrant.ApprovalProofs) > 0 && (r.Approvals == nil || r.Approvals.VerifyApprovals(ctx, actionGrant, subject) != nil) {
		return r.reject(actionGrant, ReasonApproval, nil)
	}
	if !r.Capabilities.Allows(actionGrant.Action.Capability) {
		return r.reject(actionGrant, ReasonCapability, nil)
	}
	input := policy.InputForGrant(actionGrant, subject.Issuer, subject.ID, delegation.AgentID, delegation.ID)
	if r.Policy == nil {
		return r.reject(actionGrant, ReasonPolicyDeny, errors.New("local OPA unavailable"))
	}
	evaluation, err := r.Policy.EvaluateContext(ctx, input)
	if err != nil || evaluation.Decision != "ALLOW" {
		return r.reject(actionGrant, ReasonPolicyDeny, err)
	}
	if evaluation.InputHash != actionGrant.PolicyInputHash || evaluation.PolicyHash != actionGrant.PolicyHash {
		return r.reject(actionGrant, ReasonPolicyMismatch, nil)
	}
	if r.Admission != nil {
		if err := r.Admission.Begin(); err != nil {
			switch {
			case errors.Is(err, ErrRunnerDraining):
				return r.reject(actionGrant, ReasonDraining, err)
			case errors.Is(err, ErrRunnerFrozen):
				return r.reject(actionGrant, ReasonFrozen, err)
			default:
				return r.reject(actionGrant, ReasonCapacity, err)
			}
		}
		defer r.Admission.End()
	}
	now := r.now()
	record := store.RunnerActionRecord{GrantID: actionGrant.GrantID, RunID: actionGrant.RunID, StepID: actionGrant.StepID, TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Nonce: actionGrant.Nonce, IdempotencyKey: actionGrant.IdempotencyKey, RequestHash: requestHash, Target: actionGrant.Target, Action: actionGrant.Action, CreatedAt: now, UpdatedAt: now}
	record, created, err := r.Journal.ReserveRunnerAction(ctx, record, journalAudit(auditID(actionGrant.GrantID, "reserve", 1), actionGrant.GrantID, "runner.action.reserve", "AUTHORIZED", now, map[string]any{"request_hash": requestHash}))
	if err != nil {
		return r.reject(actionGrant, ReasonJournalUnavailable, err)
	}
	if !created {
		return replayResult(actionGrant, record, requestHash)
	}
	if r.Credentials == nil || r.Adapter == nil {
		return r.failReserved(ctx, record, actionGrant, ReasonCredential, errors.New("credential or adapter unavailable"))
	}
	provider := "typed-adapter"
	if configured, ok := r.Adapter.(interface{ CredentialProvider() string }); ok {
		provider = configured.CredentialProvider()
	}
	purpose := credentials.PurposeDeploy
	if actionGrant.Action.Capability == protocol.CapabilityRollback {
		purpose = credentials.PurposeRollback
	}
	credential, err := r.Credentials.Acquire(ctx, CredentialRequest{Provider: provider, TenantID: actionGrant.TenantID, Service: actionGrant.Target.Service, Environment: actionGrant.Target.Environment, Purpose: purpose, GrantID: actionGrant.GrantID})
	if err != nil {
		return r.failReserved(ctx, record, actionGrant, ReasonCredential, err)
	}
	adapterResult, err := r.Adapter.Execute(ctx, AdapterRequest{GrantID: actionGrant.GrantID, RunID: actionGrant.RunID, StepID: actionGrant.StepID, Target: actionGrant.Target, Action: actionGrant.Action, IdempotencyKey: actionGrant.IdempotencyKey, DispatchedAt: now}, credential)
	if err != nil {
		return r.failReserved(ctx, record, actionGrant, ReasonExternalUnknown, err)
	}
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: actionGrant.GrantID, RunID: actionGrant.RunID, StepID: actionGrant.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: adapterResult.ExternalExecutionID, CompletedAt: adapterResult.CompletedAt.UTC()}
	record.Status, record.Result, record.UpdatedAt = store.RunnerActionSucceeded, result, r.now()
	if err := r.Journal.CompleteRunnerAction(ctx, record, record.StateVersion, journalAudit(auditID(actionGrant.GrantID, "complete", record.StateVersion+1), actionGrant.GrantID, "runner.action.complete", "SUCCEEDED", record.UpdatedAt, map[string]any{"external_execution_id": result.ExternalExecutionID})); err != nil {
		return r.reject(actionGrant, ReasonExternalUnknown, err)
	}
	return result, nil
}

func (r *Runner) failReserved(ctx context.Context, record store.RunnerActionRecord, actionGrant protocol.ActionGrant, reason Reason, cause error) (protocol.Result, error) {
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: actionGrant.GrantID, RunID: actionGrant.RunID, StepID: actionGrant.StepID, Status: protocol.ResultUnknown, ReasonCode: string(reason), CompletedAt: r.now()}
	record.Status, record.Result, record.UpdatedAt = store.RunnerActionUnknown, result, r.now()
	_ = r.Journal.CompleteRunnerAction(ctx, record, record.StateVersion, journalAudit(auditID(actionGrant.GrantID, "unknown", record.StateVersion+1), actionGrant.GrantID, "runner.action.complete", "UNKNOWN", record.UpdatedAt, map[string]any{"reason_code": reason}))
	return result, &Rejection{Reason: reason, Err: cause}
}

func (r *Runner) reject(grant protocol.ActionGrant, reason Reason, err error) (protocol.Result, error) {
	return protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grant.GrantID, RunID: grant.RunID, StepID: grant.StepID, Status: protocol.ResultRejected, ReasonCode: string(reason), CompletedAt: r.now()}, &Rejection{Reason: reason, Err: err}
}
func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func matchingHashes(grant protocol.ActionGrant, pinned PinnedContext) bool {
	return grant.PlanHash == pinned.PlanHash && grant.ContractHash == pinned.ContractHash && grant.ProfileHash == pinned.ProfileHash && grant.PolicyHash == pinned.PolicyHash && grant.EvidenceHash == pinned.EvidenceHash && grant.Target == pinned.Target && grant.Action == pinned.Action
}
func replayResult(grant protocol.ActionGrant, record store.RunnerActionRecord, requestHash string) (protocol.Result, error) {
	if record.RequestHash != requestHash {
		return protocol.Result{Status: protocol.ResultRejected, ReasonCode: string(ReasonReplay)}, &Rejection{Reason: ReasonReplay}
	}
	if record.Status == store.RunnerActionSucceeded {
		return record.Result, nil
	}
	return protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grant.GrantID, RunID: grant.RunID, StepID: grant.StepID, Status: protocol.ResultReconciliation, ReasonCode: string(ReasonReplay)}, &Rejection{Reason: ReasonReplay}
}
func reasonForGrant(err error) Reason {
	switch {
	case errors.Is(err, grant.ErrExpired):
		return Reason("EXPIRED_GRANT")
	case errors.Is(err, grant.ErrWrongAudience):
		return Reason("WRONG_RUNNER_AUDIENCE")
	case errors.Is(err, grant.ErrWrongTenant):
		return ReasonTenant
	default:
		return ReasonIdentity
	}
}
