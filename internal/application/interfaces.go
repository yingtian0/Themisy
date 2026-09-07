package application

import (
	"context"

	"themisy/internal/domain"
	workflowcore "themisy/internal/workflow"
)

type ReleaseService interface {
	Services() []domain.Service
	Plan(domain.ReleaseRequest) (domain.ReleasePlan, error)
	PlanIntent(domain.ReleaseIntent) (domain.ReleasePlan, error)
	Start(context.Context, domain.ReleaseRequest) (*domain.ReleaseRun, bool, error)
	StartIntent(context.Context, domain.ReleaseIntent) (*domain.ReleaseRun, bool, error)
	Get(string) (*domain.ReleaseRun, error)
	Events(string) ([]domain.AuditEvent, error)
	DecideApproval(context.Context, string, string, string, []string, bool) (*domain.ReleaseRun, error)
	Cancel(string, string) (*domain.ReleaseRun, error)
	Pause(string, string) (*domain.ReleaseRun, error)
	Resume(context.Context, string, string) (*domain.ReleaseRun, error)
}

// ControlPlane is the single safe use-case boundary shared by REST, CLI (through
// REST), MCP, and UI. It intentionally contains no adapter operation.
type ControlPlane interface {
	Services() []domain.Service
	PlanIntentForTenant(string, domain.ReleaseIntent) (domain.ReleasePlan, error)
	GetPlan(string, string) (domain.ReleasePlan, error)
	StartIntentForTenant(context.Context, string, domain.ReleaseIntent) (*domain.ReleaseRun, bool, error)
	GetForTenant(string, string) (*domain.ReleaseRun, error)
	EventsForTenant(string, string) ([]domain.AuditEvent, error)
	ControlForTenant(context.Context, string, string, string, string) (*domain.ReleaseRun, error)
	ListPendingApprovals(string) ([]domain.ApprovalSummary, error)
	DecideApprovalForTenant(context.Context, string, string, string, string, []string, bool) (*domain.ReleaseRun, error)
	RevokeApprovalForTenant(context.Context, string, string, string, string, []string) (*domain.ReleaseRun, error)
	ValidateContract([]byte) (domain.ContractValidation, error)
	ListRunners(string) []domain.RunnerInfo
	FreezeRunner(string, string, string) (domain.RunnerInfo, error)
	DrainRunner(string, string, string) (domain.RunnerInfo, error)
	ActivateRunner(string, string, string) (domain.RunnerInfo, error)
}

type WorkflowController interface {
	StartRelease(context.Context, workflowcore.ReleaseInput) (workflowcore.Execution, error)
	SignalApproval(context.Context, string, workflowcore.ApprovalSignal) error
	Pause(context.Context, string, workflowcore.ControlSignal) error
	Resume(context.Context, string, workflowcore.ControlSignal) error
	Cancel(context.Context, string, workflowcore.ControlSignal) error
}
