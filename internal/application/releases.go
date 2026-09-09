package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
)

var ErrInvalidRequest = errors.New("invalid release request")

type Releases struct {
	planner       *planner.Planner
	store         store.DurableStore
	workflows     WorkflowController
	now           func() time.Time
	mu            sync.RWMutex
	runners       map[string]domain.RunnerInfo
	frozenRunners map[string]domain.RunnerInfo
}

func NewReleases(releasePlanner *planner.Planner, st store.DurableStore, workflows WorkflowController) *Releases {
	r := &Releases{planner: releasePlanner, store: st, workflows: workflows, now: time.Now, runners: make(map[string]domain.RunnerInfo), frozenRunners: make(map[string]domain.RunnerInfo)}
	for _, service := range releasePlanner.Services() {
		for _, group := range service.RunnerGroups {
			if group != "" {
				r.runners[group] = domain.RunnerInfo{ID: group, Group: group, Status: domain.RunnerReady, Capacity: 20}
			}
		}
	}
	return r
}

func (r *Releases) Services() []domain.Service { return r.planner.Services() }
func (r *Releases) Plan(request domain.ReleaseRequest) (domain.ReleasePlan, error) {
	if err := validateRequest(request); err != nil {
		return domain.ReleasePlan{}, err
	}
	plan, err := r.planner.Plan(request)
	if err == nil {
		err = r.store.SavePlan(plan)
	}
	return plan, err
}
func (r *Releases) PlanIntent(intent domain.ReleaseIntent) (domain.ReleasePlan, error) {
	request := planner.LegacyRequestFromIntent(intent)
	if err := validateRequest(request); err != nil {
		return domain.ReleasePlan{}, err
	}
	plan, err := r.planner.PlanIntent(intent)
	if err == nil {
		err = r.store.SavePlan(plan)
	}
	return plan, err
}

func (r *Releases) PlanIntentForTenant(tenant string, intent domain.ReleaseIntent) (domain.ReleasePlan, error) {
	if err := bindTenant(tenant, &intent.TenantID); err != nil {
		return domain.ReleasePlan{}, err
	}
	return r.PlanIntent(intent)
}

func (r *Releases) GetPlan(tenant, id string) (domain.ReleasePlan, error) {
	plan, err := r.store.GetPlan(id)
	if err != nil {
		return domain.ReleasePlan{}, err
	}
	if planTenant(plan) != normalized(tenant, "default") {
		return domain.ReleasePlan{}, tenantBoundaryError()
	}
	return plan, nil
}

func (r *Releases) StartIntentForTenant(ctx context.Context, tenant string, intent domain.ReleaseIntent) (*domain.ReleaseRun, bool, error) {
	if err := bindTenant(tenant, &intent.TenantID); err != nil {
		return nil, false, err
	}
	return r.StartIntent(ctx, intent)
}
func (r *Releases) Start(ctx context.Context, request domain.ReleaseRequest) (*domain.ReleaseRun, bool, error) {
	plan, err := r.Plan(request)
	if err != nil {
		return nil, false, err
	}
	return r.start(ctx, request, plan)
}
func (r *Releases) StartIntent(ctx context.Context, intent domain.ReleaseIntent) (*domain.ReleaseRun, bool, error) {
	plan, err := r.PlanIntent(intent)
	if err != nil {
		return nil, false, err
	}
	return r.start(ctx, planner.LegacyRequestFromIntent(intent), plan)
}

func (r *Releases) start(ctx context.Context, request domain.ReleaseRequest, plan domain.ReleasePlan) (*domain.ReleaseRun, bool, error) {
	if err := r.planner.ValidatePlan(plan, r.now()); err != nil {
		return nil, false, fmt.Errorf("validate release plan before workflow start: %w", err)
	}
	now := r.now().UTC()
	runID := newID("run")
	run := &domain.ReleaseRun{ID: runID, WorkflowID: runID, RequestID: request.RequestID, ReleaseVersion: request.ReleaseVersion, TenantID: normalized(request.TenantID, "default"), Environment: request.Environment, Region: normalized(request.Region, "global"), Cluster: request.Cluster, RequestedBy: request.RequestedBy, SubjectType: request.SubjectType, SubjectIssuer: request.SubjectIssuer, UserIdentityProof: request.UserIdentityProof, DelegationRef: request.DelegationRef, Agent: request.Agent, Plan: plan, Status: domain.RunPending, StateVersion: 1, CreatedAt: now, UpdatedAt: now}
	changes := make(map[string]domain.Change, len(request.Changes))
	for _, change := range request.Changes {
		changes[change.Service] = change
	}
	for _, phase := range plan.Phases {
		for _, planned := range phase.Steps {
			run.Steps = append(run.Steps, domain.ReleaseStep{Service: planned.Service, Phase: phase.Number, Status: domain.StepPending, Change: changes[planned.Service], VerificationRequired: planned.VerificationRequired, ObservationWindow: planned.ObservationWindow, RollbackMode: planned.RollbackMode})
		}
	}
	audit := domain.AuditEvent{ID: newID("audit"), CorrelationID: run.ID, ActorType: "user", ActorID: request.RequestedBy, DelegatedBy: request.RequestedBy, Action: "release.start", ResourceType: "release_run", ResourceID: run.ID, Result: "ACCEPTED", Details: map[string]any{"plan_hash": plan.Hash, "workflow_id": run.WorkflowID}, Timestamp: now}
	outbox := domain.OutboxEvent{ID: newID("workflow"), AggregateType: "release_run", AggregateID: run.ID, EventType: "release.workflow.requested", Payload: map[string]any{"workflow_id": run.WorkflowID}, CreatedAt: now, AvailableAt: now}
	stored, created, err := r.store.CreateRunAtomic(run, []domain.AuditEvent{audit}, []domain.OutboxEvent{outbox})
	if err != nil {
		return nil, false, err
	}
	if !created {
		return stored, false, nil
	}
	if _, err := r.workflows.StartRelease(ctx, workflowcore.ReleaseInput{Run: *stored}); err != nil {
		return stored, created, fmt.Errorf("start temporal release workflow: %w", err)
	}
	if err := r.store.MarkOutboxPublished(outbox.ID, r.now().UTC()); err != nil {
		return stored, created, fmt.Errorf("mark workflow request published: %w", err)
	}
	return stored, created, nil
}

func (r *Releases) Get(id string) (*domain.ReleaseRun, error) { return r.store.GetRun(id) }

func (r *Releases) GetForTenant(tenant, id string) (*domain.ReleaseRun, error) {
	run, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if normalized(run.TenantID, "default") != normalized(tenant, "default") {
		return nil, tenantBoundaryError()
	}
	return run, nil
}
func (r *Releases) Events(id string) ([]domain.AuditEvent, error) {
	if _, err := r.store.GetRun(id); err != nil {
		return nil, err
	}
	return r.store.AuditEvents(id)
}

func (r *Releases) EventsForTenant(tenant, id string) ([]domain.AuditEvent, error) {
	if _, err := r.GetForTenant(tenant, id); err != nil {
		return nil, err
	}
	return r.store.AuditEvents(id)
}

func (r *Releases) ValidateContract(data []byte) (domain.ContractValidation, error) {
	decoded, err := contract.Decode(data)
	if err != nil {
		return domain.ContractValidation{Valid: false}, err
	}
	return domain.ContractValidation{Valid: true, Name: decoded.Metadata.Name, ContentHash: decoded.ContentHash}, nil
}

func (r *Releases) ListRunners(tenant string) []domain.RunnerInfo {
	tenant = normalized(tenant, "default")
	fleet, fleetErr := r.store.ListRunnerFleet(context.Background(), tenant)
	if fleetErr == nil && len(fleet) > 0 {
		now := r.now().UTC()
		for index := range fleet {
			if fleet[index].LastSeen.Add(90 * time.Second).Before(now) {
				fleet[index].Status = domain.RunnerUnknown
				fleet[index].Capacity = 0
			}
		}
		return fleet
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	runners := make([]domain.RunnerInfo, 0, len(r.runners))
	for _, runner := range r.runners {
		runner.TenantID = tenant
		if frozen, ok := r.frozenRunners[tenant+"\x00"+runner.ID]; ok {
			runner = frozen
		}
		runners = append(runners, runner)
	}
	if fleetErr != nil {
		for index := range runners {
			runners[index].Status = domain.RunnerUnknown
			runners[index].Capacity = 0
		}
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].ID < runners[j].ID })
	return runners
}

func (r *Releases) RunnerCapacity(tenant, id string) int {
	tenant = normalized(tenant, "default")
	fleet, err := r.store.ListRunnerFleet(context.Background(), tenant)
	if err != nil {
		return 0
	}
	if len(fleet) > 0 {
		now := r.now().UTC()
		capacity := 0
		for _, runner := range fleet {
			if runner.Group == id && runner.Status == domain.RunnerReady && runner.LastSeen.Add(90*time.Second).After(now) {
				capacity += runner.Capacity
			}
		}
		return capacity
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if frozen, ok := r.frozenRunners[tenant+"\x00"+id]; ok {
		return frozen.Capacity
	}
	if runner, ok := r.runners[id]; ok && runner.Status == domain.RunnerReady {
		return runner.Capacity
	}
	return 0
}

func (r *Releases) FreezeRunner(tenant, id, actor string) (domain.RunnerInfo, error) {
	return r.setRunnerStatus(tenant, id, actor, domain.RunnerFrozen)
}

func (r *Releases) DrainRunner(tenant, id, actor string) (domain.RunnerInfo, error) {
	return r.setRunnerStatus(tenant, id, actor, domain.RunnerDraining)
}

func (r *Releases) ActivateRunner(tenant, id, actor string) (domain.RunnerInfo, error) {
	return r.setRunnerStatus(tenant, id, actor, domain.RunnerReady)
}

func (r *Releases) setRunnerStatus(tenant, id, actor string, status domain.RunnerStatus) (domain.RunnerInfo, error) {
	if actor == "" {
		return domain.RunnerInfo{}, errors.New("runner control actor is required")
	}
	tenant = normalized(tenant, "default")
	now := r.now().UTC()
	action := "runner." + strings.ToLower(string(status))
	if status == domain.RunnerReady {
		action = "runner.activate"
	}
	audit := domain.AuditEvent{ID: newID("audit"), CorrelationID: "runner/" + tenant + "/" + id, ActorType: "user", ActorID: actor, Action: action, ResourceType: "runner", ResourceID: id, Result: string(status), Timestamp: now}
	updated, err := r.store.SetRunnerStatus(context.Background(), tenant, id, status, actor, now, audit)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return updated, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	runner, ok := r.runners[id]
	if !ok {
		return domain.RunnerInfo{}, store.ErrNotFound
	}
	runner.TenantID = tenant
	runner.Status = status
	runner.ControlledBy = actor
	runner.ControlledAt = &now
	if status == domain.RunnerReady {
		runner.Capacity = runner.ReportedCapacity
		if runner.Capacity == 0 {
			runner.Capacity = 20
		}
		delete(r.frozenRunners, runner.TenantID+"\x00"+id)
		if err := r.store.AppendAudit(audit); err != nil {
			return domain.RunnerInfo{}, err
		}
		return runner, nil
	}
	runner.Capacity = 0
	if status == domain.RunnerFrozen {
		runner.FrozenBy = actor
		runner.FrozenAt = &now
	}
	r.frozenRunners[runner.TenantID+"\x00"+id] = runner
	if err := r.store.AppendAudit(audit); err != nil {
		return domain.RunnerInfo{}, err
	}
	return runner, nil
}

func planTenant(plan domain.ReleasePlan) string {
	for _, phase := range plan.Phases {
		for _, step := range phase.Steps {
			return normalized(step.Scheduling.TenantID, "default")
		}
	}
	return "default"
}

func bindTenant(authenticated string, requested *string) error {
	authenticated = normalized(authenticated, "default")
	if *requested == "" {
		*requested = authenticated
		return nil
	}
	if *requested != authenticated {
		return tenantBoundaryError()
	}
	return nil
}

func tenantBoundaryError() error {
	return domain.NewReasonError(domain.ReasonTenantBoundary, "tenant_id", "resource is outside the authenticated tenant", nil)
}

func validateRequest(request domain.ReleaseRequest) error {
	switch {
	case request.RequestID == "":
		return fmt.Errorf("%w: request_id is required", ErrInvalidRequest)
	case request.ReleaseVersion == "":
		return fmt.Errorf("%w: release_version is required", ErrInvalidRequest)
	case request.RequestedBy == "":
		return fmt.Errorf("%w: requested_by is required", ErrInvalidRequest)
	case request.Agent.ID == "":
		return fmt.Errorf("%w: delegated_agent.id is required", ErrInvalidRequest)
	default:
		return nil
	}
}
func newID(prefix string) string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(value)
}

func normalized(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ ReleaseService = (*Releases)(nil)
var _ ControlPlane = (*Releases)(nil)
