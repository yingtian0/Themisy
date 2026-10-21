package ui

import (
	"themisy/internal/domain"
	"time"
)

type runView struct {
	Title  string
	Run    *domain.ReleaseRun
	Events []domain.AuditEvent
}

type approvalView struct {
	Title     string
	Approvals []approvalCard
	CSRFToken string
}

type approvalCard struct {
	domain.ApprovalSummary
	Run            *domain.ReleaseRun
	Step           *domain.ReleaseStep
	Planned        *domain.PlanStep
	BindingProblem string
	Expired        bool
	PlanExpired    bool
	Reviewable     bool
}

func makeApprovalCard(summary domain.ApprovalSummary, run *domain.ReleaseRun, now time.Time) approvalCard {
	card := approvalCard{ApprovalSummary: summary}
	if run == nil || run.ID != summary.RunID || run.Plan.Hash == "" || summary.Approval.PlanHash != run.Plan.Hash || run.Environment != run.Plan.Environment || (run.Environment != domain.EnvironmentStaging && run.Environment != domain.EnvironmentProduction) {
		card.BindingProblem = "Approval is not bound to an available matching Plan. Refresh and request a new review."
		return card
	}
	for i := range run.Steps {
		step := &run.Steps[i]
		if step.Service == summary.Service && step.Approval != nil && step.Approval.ID == summary.Approval.ID && step.Approval.PlanHash == run.Plan.Hash {
			if card.Step != nil {
				card.BindingProblem = "Ambiguous approved service mapping."
				return card
			}
			card.Step = step
		}
	}
	for i := range run.Plan.Phases {
		for j := range run.Plan.Phases[i].Steps {
			planned := &run.Plan.Phases[i].Steps[j]
			if planned.Service == summary.Service {
				if card.Planned != nil {
					card.BindingProblem = "Ambiguous pinned Plan step."
					return card
				}
				card.Planned = planned
			}
		}
	}
	if card.Step == nil || card.Planned == nil || card.Planned.DesiredVersion == "" || card.Planned.DesiredVersion != card.Step.Change.DesiredVersion {
		card.BindingProblem = "Pinned change is unavailable or differs from the approval's Run. Do not infer a replacement target."
		return card
	}
	card.Run = run
	card.Approval = *card.Step.Approval
	card.Expired = card.Approval.ExpiresAt.IsZero() || !now.Before(card.Approval.ExpiresAt)
	card.PlanExpired = run.Plan.ExpiresAt.IsZero() || !now.Before(run.Plan.ExpiresAt)
	card.Reviewable = !card.Expired && !card.PlanExpired && card.Approval.Status == domain.ApprovalPending
	return card
}
