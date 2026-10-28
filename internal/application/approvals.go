package application

import (
	"context"
	"errors"
	"fmt"

	"themisy/internal/domain"
	"themisy/internal/identity"
	workflowcore "themisy/internal/workflow"
)

var ErrApproval = errors.New("approval rejected")

func (r *Releases) DecideApproval(ctx context.Context, runID, approvalID, actor string, roles []string, approve bool) (*domain.ReleaseRun, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: approval actor is required", ErrApproval)
	}
	run, err := r.store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	var target *domain.Approval
	for index := range run.Steps {
		if run.Steps[index].Approval != nil && run.Steps[index].Approval.ID == approvalID {
			target = run.Steps[index].Approval
			break
		}
	}
	if target == nil || target.Status != domain.ApprovalPending {
		return nil, fmt.Errorf("%w: pending approval not found", ErrApproval)
	}
	if target.PlanHash != run.Plan.Hash {
		return nil, fmt.Errorf("%w: plan changed after approval request", ErrApproval)
	}
	if !r.now().Before(target.ExpiresAt) {
		return nil, fmt.Errorf("%w: approval expired", ErrApproval)
	}
	if approve && actor == run.RequestedBy {
		return nil, fmt.Errorf("%w: requester cannot approve their own release", ErrApproval)
	}
	if approve && !containsAll(roles, target.RequiredRoles) {
		return nil, fmt.Errorf("%w: approver lacks required roles %v", ErrApproval, target.RequiredRoles)
	}
	issuer := ""
	if r.Authority != nil {
		verified, ok := identity.RequestActorFromContext(ctx)
		if !ok || verified.TenantID != run.TenantID || verified.Subject != actor || !containsAll(verified.Roles, target.RequiredRoles) {
			return nil, ErrApproval
		}
		issuer = verified.Issuer
		if approve && verified.Subject == run.RequestedBy && verified.Issuer == run.SubjectIssuer {
			return nil, ErrApproval
		}
	}
	action := "deny"
	if approve {
		action = "approve"
	}
	if err := r.workflows.SignalApproval(ctx, run.WorkflowID, workflowcore.ApprovalSignal{ApprovalID: approvalID, Actor: actor, Issuer: issuer, Roles: roles, Approve: approve, Action: action}); err != nil {
		return nil, err
	}
	return r.store.GetRun(runID)
}

func (r *Releases) DecideApprovalForTenant(ctx context.Context, tenant, runID, approvalID, actor string, roles []string, approve bool) (*domain.ReleaseRun, error) {
	if _, err := r.GetForTenant(tenant, runID); err != nil {
		return nil, err
	}
	return r.DecideApproval(ctx, runID, approvalID, actor, roles, approve)
}

func (r *Releases) RevokeApprovalForTenant(ctx context.Context, tenant, runID, approvalID, actor string, roles []string) (*domain.ReleaseRun, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: approval actor is required", ErrApproval)
	}
	run, err := r.GetForTenant(tenant, runID)
	if err != nil {
		return nil, err
	}
	var target *domain.Approval
	for index := range run.Steps {
		if run.Steps[index].Approval != nil && run.Steps[index].Approval.ID == approvalID {
			target = run.Steps[index].Approval
			break
		}
	}
	if target == nil || target.Status != domain.ApprovalPending {
		return nil, fmt.Errorf("%w: only a pending approval can be revoked safely", ErrApproval)
	}
	if !containsAll(roles, target.RequiredRoles) {
		return nil, fmt.Errorf("%w: revoker lacks required roles %v", ErrApproval, target.RequiredRoles)
	}
	if err := r.workflows.SignalApproval(ctx, run.WorkflowID, workflowcore.ApprovalSignal{ApprovalID: approvalID, Actor: actor, Roles: roles, Action: "revoke"}); err != nil {
		return nil, err
	}
	return r.store.GetRun(runID)
}

func (r *Releases) ListPendingApprovals(tenant string) ([]domain.ApprovalSummary, error) {
	runs, err := r.store.ListRuns()
	if err != nil {
		return nil, err
	}
	tenant = normalized(tenant, "default")
	result := make([]domain.ApprovalSummary, 0)
	for _, run := range runs {
		if normalized(run.TenantID, "default") != tenant {
			continue
		}
		for _, step := range run.Steps {
			if step.Approval != nil && step.Approval.Status == domain.ApprovalPending {
				result = append(result, domain.ApprovalSummary{Approval: *step.Approval, RunID: run.ID, Service: step.Service, TenantID: tenant})
			}
		}
	}
	return result, nil
}

func containsAll(actual, required []string) bool {
	set := make(map[string]bool, len(actual))
	for _, value := range actual {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func (r *Releases) Cancel(runID, actor string) (*domain.ReleaseRun, error) {
	return r.control(context.Background(), runID, actor, "cancel")
}
func (r *Releases) Pause(runID, actor string) (*domain.ReleaseRun, error) {
	return r.control(context.Background(), runID, actor, "pause")
}
func (r *Releases) Resume(ctx context.Context, runID, actor string) (*domain.ReleaseRun, error) {
	return r.control(ctx, runID, actor, "resume")
}

func (r *Releases) ControlForTenant(ctx context.Context, tenant, runID, actor, action string) (*domain.ReleaseRun, error) {
	if _, err := r.GetForTenant(tenant, runID); err != nil {
		return nil, err
	}
	return r.control(ctx, runID, actor, action)
}

func (r *Releases) control(ctx context.Context, runID, actor, action string) (*domain.ReleaseRun, error) {
	if actor == "" {
		return nil, fmt.Errorf("actor is required")
	}
	run, err := r.store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	signal := workflowcore.ControlSignal{Actor: actor}
	switch action {
	case "cancel":
		err = r.workflows.Cancel(ctx, run.WorkflowID, signal)
	case "pause":
		err = r.workflows.Pause(ctx, run.WorkflowID, signal)
	case "resume":
		err = r.workflows.Resume(ctx, run.WorkflowID, signal)
	default:
		return nil, fmt.Errorf("unsupported release control action %q", action)
	}
	if err != nil {
		return nil, err
	}
	return r.store.GetRun(runID)
}
