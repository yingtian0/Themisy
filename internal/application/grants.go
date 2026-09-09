package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

var ErrGrantNotAuthorized = errors.New("action grant is not authorized")

type GrantIssueRequest struct {
	RunID               string
	StepID              string
	Capability          protocol.Capability
	ExternalExecutionID string
	IdempotencyKey      string
}

// GrantExecutor connects Temporal activities to the outbound Runner path.
// Verification remains delegated to the configured read-only verifier.
type GrantExecutor struct {
	Grants       *Grants
	Verification executor.ReleaseExecutor
	PollInterval time.Duration
}

func (e *GrantExecutor) Deploy(ctx context.Context, request executor.DeployRequest) (executor.Deployment, error) {
	if e.Grants == nil {
		return executor.Deployment{}, errors.New("grant executor is unavailable")
	}
	record, _, err := e.Grants.Issue(ctx, GrantIssueRequest{RunID: request.RunID, StepID: request.Service, Capability: protocol.CapabilityDeploy, IdempotencyKey: request.IdempotencyKey})
	if err != nil {
		return executor.Deployment{}, err
	}
	result, err := e.Grants.WaitResult(ctx, record.Grant.GrantID, e.PollInterval)
	if err != nil {
		return executor.Deployment{}, err
	}
	if result.Status != protocol.ResultSucceeded {
		return executor.Deployment{}, fmt.Errorf("runner result %s: %s", result.Status, result.ReasonCode)
	}
	return executor.Deployment{ExternalID: result.ExternalExecutionID, StartedAt: record.Grant.IssuedAt, FinishedAt: result.CompletedAt, RunID: request.RunID, Service: request.Service, Environment: request.Environment, DesiredVersion: request.DesiredVersion, IdempotencyKey: request.IdempotencyKey}, nil
}

func (e *GrantExecutor) Rollback(ctx context.Context, deployment executor.Deployment) (executor.Deployment, error) {
	if e.Grants == nil || deployment.RunID == "" || deployment.Service == "" {
		return executor.Deployment{}, errors.New("rollback grant context is unavailable")
	}
	record, _, err := e.Grants.Issue(ctx, GrantIssueRequest{RunID: deployment.RunID, StepID: deployment.Service, Capability: protocol.CapabilityRollback, ExternalExecutionID: deployment.ExternalID, IdempotencyKey: deployment.IdempotencyKey + "/rollback"})
	if err != nil {
		return executor.Deployment{}, err
	}
	result, err := e.Grants.WaitResult(ctx, record.Grant.GrantID, e.PollInterval)
	if err != nil {
		return executor.Deployment{}, err
	}
	if result.Status != protocol.ResultSucceeded {
		return executor.Deployment{}, fmt.Errorf("runner rollback result %s: %s", result.Status, result.ReasonCode)
	}
	return executor.Deployment{ExternalID: result.ExternalExecutionID, StartedAt: record.Grant.IssuedAt, FinishedAt: result.CompletedAt, RunID: deployment.RunID, Service: deployment.Service, Environment: deployment.Environment, DesiredVersion: deployment.DesiredVersion, IdempotencyKey: deployment.IdempotencyKey + "/rollback"}, nil
}

func (e *GrantExecutor) Verify(ctx context.Context, deployment executor.Deployment) (executor.VerificationResult, error) {
	if e.Verification == nil {
		return executor.VerificationResult{}, errors.New("verification executor is unavailable")
	}
	return e.Verification.Verify(ctx, deployment)
}

type Grants struct {
	Store  store.DurableStore
	Signer grant.Signer
	Issuer string
	TTL    time.Duration
	Now    func() time.Time
}

func NewGrants(st store.DurableStore, signer grant.Signer, issuer string, ttl time.Duration) (*Grants, error) {
	if st == nil || signer == nil || issuer == "" {
		return nil, errors.New("grant store, signer, and issuer are required")
	}
	if ttl <= 0 || ttl > 15*time.Minute {
		return nil, errors.New("grant TTL must be between zero and fifteen minutes")
	}
	return &Grants{Store: st, Signer: signer, Issuer: issuer, TTL: ttl, Now: time.Now}, nil
}

// Issue creates a narrowly typed Action Grant from durable, already-authorized
// run state. It never accepts a cloud resource identifier or provider payload.
func (g *Grants) Issue(ctx context.Context, request GrantIssueRequest) (store.GrantDispatchRecord, bool, error) {
	if request.RunID == "" || request.StepID == "" {
		return store.GrantDispatchRecord{}, false, fmt.Errorf("%w: run and step are required", ErrGrantNotAuthorized)
	}
	run, err := g.Store.GetRun(request.RunID)
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	step, planned, err := authorizedStep(*run, request.StepID, request.Capability, g.now())
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = fmt.Sprintf("%s/%s/%s/%s", run.ID, run.Environment, step.Service, step.Change.DesiredVersion)
		if request.Capability == protocol.CapabilityRollback {
			request.IdempotencyKey += "/rollback"
		}
	}
	now := g.now()
	expiresAt := now.Add(g.TTL)
	if !run.Plan.ExpiresAt.IsZero() && run.Plan.ExpiresAt.Before(expiresAt) {
		expiresAt = run.Plan.ExpiresAt
	}
	if !expiresAt.After(now) {
		return store.GrantDispatchRecord{}, false, fmt.Errorf("%w: plan or grant expired", ErrGrantNotAuthorized)
	}
	approvalProofs := []string{}
	if step.Approval != nil && step.Approval.Status == domain.ApprovalApproved {
		approvalProofs = append(approvalProofs, step.Approval.ID)
	}
	risk := step.Change.Risk
	if risk == "" {
		risk = planned.Scheduling.RiskTier
	}
	actionGrant := protocol.ActionGrant{
		ProtocolVersion:   protocol.VersionV1Alpha1,
		GrantID:           newID("grant"),
		Issuer:            g.Issuer,
		TenantID:          run.TenantID,
		RunnerGroup:       planned.Scheduling.RunnerGroup,
		RunID:             run.ID,
		StepID:            step.Service,
		SubjectType:       run.SubjectType,
		UserSubject:       run.RequestedBy,
		UserIdentityProof: run.UserIdentityProof,
		AgentID:           run.Agent.ID,
		DelegationRef:     run.DelegationRef,
		Target:            protocol.Target{Service: step.Service, Environment: string(run.Environment)},
		Action:            protocol.Action{Capability: request.Capability, ArtifactDigest: step.Change.DesiredVersion, ExternalExecutionID: request.ExternalExecutionID},
		Risk:              risk,
		PlanHash:          run.Plan.Hash,
		ContractHash:      planned.ContractHash,
		ProfileHash:       planned.ProfileHash,
		PolicyHash:        run.Plan.PolicyHash,
		PolicyInputHash:   step.Policy.InputHash,
		EvidenceHash:      run.Plan.EvidenceHash,
		ApprovalProofs:    approvalProofs,
		IdempotencyKey:    request.IdempotencyKey,
		Nonce:             newID("nonce"),
		IssuedAt:          now,
		ExpiresAt:         expiresAt,
	}
	signed, err := grant.SignActionGrant(ctx, g.Signer, actionGrant)
	if err != nil {
		return store.GrantDispatchRecord{}, false, fmt.Errorf("sign action grant: %w", err)
	}
	outboxID := "grant-dispatch/" + signed.GrantID
	record := store.GrantDispatchRecord{Grant: signed, OutboxID: outboxID, CreatedAt: now, UpdatedAt: now}
	audit := domain.AuditEvent{ID: newID("audit"), CorrelationID: run.ID, ActorType: "system", ActorID: "grant-issuer", DelegatedBy: run.RequestedBy, Action: "grant.issue", ResourceType: "action_grant", ResourceID: signed.GrantID, Result: "AUTHORIZED", Details: map[string]any{"step_id": step.Service, "runner_group": signed.RunnerGroup, "capability": signed.Action.Capability, "plan_hash": signed.PlanHash, "policy_hash": signed.PolicyHash}, Timestamp: now}
	outbox := domain.OutboxEvent{ID: outboxID, AggregateType: "action_grant", AggregateID: signed.GrantID, EventType: "grant.dispatch.requested", Payload: map[string]any{"grant_id": signed.GrantID, "tenant_id": signed.TenantID, "runner_group": signed.RunnerGroup}, CreatedAt: now, AvailableAt: now}
	created, fresh, err := g.Store.CreateGrantDispatch(ctx, record, audit, outbox)
	if err != nil {
		return store.GrantDispatchRecord{}, false, fmt.Errorf("persist action grant: %w", err)
	}
	return created, fresh, nil
}

func (g *Grants) WaitResult(ctx context.Context, grantID string, interval time.Duration) (protocol.Result, error) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		record, err := g.Store.GetGrantDispatch(ctx, grantID)
		if err != nil {
			return protocol.Result{}, err
		}
		switch record.Status {
		case store.GrantDispatchSucceeded, store.GrantDispatchRejected, store.GrantDispatchUnknown:
			return record.Result, nil
		}
		select {
		case <-ctx.Done():
			return protocol.Result{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func authorizedStep(run domain.ReleaseRun, stepID string, capability protocol.Capability, now time.Time) (domain.ReleaseStep, domain.PlanStep, error) {
	if run.Status != domain.RunRunning || run.SubjectType == "" || run.RequestedBy == "" || run.UserIdentityProof == "" || run.Agent.ID == "" || run.DelegationRef == "" {
		return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: active run identity and delegation are required", ErrGrantNotAuthorized)
	}
	if capability != protocol.CapabilityDeploy && capability != protocol.CapabilityRollback {
		return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: unsupported capability", ErrGrantNotAuthorized)
	}
	var step *domain.ReleaseStep
	for index := range run.Steps {
		if run.Steps[index].Service == stepID {
			step = &run.Steps[index]
			break
		}
	}
	if step == nil || (capability == protocol.CapabilityDeploy && step.Status != domain.StepExecuting) || (capability == protocol.CapabilityRollback && step.Status != domain.StepRollingBack) {
		return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: step is not dispatchable", ErrGrantNotAuthorized)
	}
	if step.Policy == nil || step.Policy.InputHash == "" || (step.Policy.Decision != domain.DecisionAllow && step.Policy.Decision != domain.DecisionRequireApproval) {
		return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: policy did not authorize the step", ErrGrantNotAuthorized)
	}
	if step.Policy.Decision == domain.DecisionRequireApproval {
		if step.Approval == nil || step.Approval.Status != domain.ApprovalApproved || step.Approval.PlanHash != run.Plan.Hash || step.Approval.DecidedAt == nil || !step.Approval.ExpiresAt.After(now) {
			return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: valid approval is required", ErrGrantNotAuthorized)
		}
	}
	var planned *domain.PlanStep
	for _, phase := range run.Plan.Phases {
		for index := range phase.Steps {
			if phase.Steps[index].Service == stepID {
				copy := phase.Steps[index]
				planned = &copy
				break
			}
		}
	}
	if planned == nil || planned.Scheduling.RunnerGroup == "" || planned.ContractHash == "" || planned.ProfileHash == "" || run.Plan.Hash == "" || run.Plan.PolicyHash == "" || run.Plan.EvidenceHash == "" {
		return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: pinned execution context is incomplete", ErrGrantNotAuthorized)
	}
	for _, dependency := range planned.Dependencies {
		if !dependency.Type.EnforcesRolloutOrder() || dependency.Service == "" {
			continue
		}
		found := false
		for _, candidate := range run.Steps {
			if candidate.Service == dependency.Service && candidate.Status == domain.StepSucceeded {
				found = true
				break
			}
		}
		if !found {
			return domain.ReleaseStep{}, domain.PlanStep{}, fmt.Errorf("%w: dependency %s has not succeeded", ErrGrantNotAuthorized, dependency.Service)
		}
	}
	return *step, *planned, nil
}

func (g *Grants) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}
