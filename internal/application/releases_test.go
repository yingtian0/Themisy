package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
)

func TestStartPersistsWorkflowRequestAndDeduplicatesCommand(t *testing.T) {
	p, err := planner.New([]domain.Service{{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	controller := &fakeWorkflowController{}
	service := NewReleases(p, memory, controller)
	request := applicationRequest()
	run, created, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !created || run.Status != domain.RunPending || run.WorkflowID == "" {
		t.Fatalf("created=%v run=%#v", created, run)
	}
	if controller.starts != 1 {
		t.Fatalf("workflow starts=%d", controller.starts)
	}
	duplicate, created, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created || duplicate.ID != run.ID || controller.starts != 1 {
		t.Fatalf("duplicate=%#v created=%v starts=%d", duplicate, created, controller.starts)
	}
	events, err := memory.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events=%d", len(events))
	}
	outbox, err := memory.PendingOutbox(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 || outbox[0].EventType != "release.run.project" {
		t.Fatalf("pending outbox=%#v, want only rebuildable projection", outbox)
	}
}

func TestApprovalValidationHappensBeforeTemporalSignal(t *testing.T) {
	p, _ := planner.New([]domain.Service{{Name: "identity"}})
	memory := store.NewMemory()
	controller := &fakeWorkflowController{}
	service := NewReleases(p, memory, controller)
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "approval-run", WorkflowID: "approval-run", RequestID: "approval-request", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Status: domain.StepWaitingApproval, Approval: &domain.Approval{ID: "approval", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DecideApproval(context.Background(), run.ID, "approval", "requester", []string{"sre"}, true); !errors.Is(err, ErrApproval) {
		t.Fatalf("self approval error=%v", err)
	}
	if _, err := service.DecideApproval(context.Background(), run.ID, "approval", "other", nil, true); !errors.Is(err, ErrApproval) {
		t.Fatalf("role error=%v", err)
	}
	if controller.approvals != 0 {
		t.Fatalf("invalid signals=%d", controller.approvals)
	}
	if _, err := service.DecideApproval(context.Background(), run.ID, "approval", "other", []string{"sre"}, true); err != nil {
		t.Fatal(err)
	}
	if controller.approvals != 1 {
		t.Fatalf("valid signals=%d", controller.approvals)
	}
}

func TestWorkflowStartOutboxRecoversAfterControlPlaneFailure(t *testing.T) {
	p, err := planner.New([]domain.Service{{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	controller := &fakeWorkflowController{err: errors.New("temporal unavailable")}
	service := NewReleases(p, memory, controller)
	run, created, err := service.Start(context.Background(), applicationRequest())
	if err == nil || !created || run == nil {
		t.Fatalf("run=%#v created=%v err=%v", run, created, err)
	}
	controller.err = nil
	published, err := service.RecoverPendingWorkflows(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || controller.starts != 2 {
		t.Fatalf("published=%d starts=%d", published, controller.starts)
	}
	pending, err := memory.PendingOutboxByType(workflowRequestedEvent, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending workflow requests=%d", len(pending))
	}
}

func TestControlPlaneUseCasesEnforceTenantBoundary(t *testing.T) {
	p, err := planner.New([]domain.Service{{Name: "identity", RunnerGroups: map[domain.Environment]string{domain.EnvironmentStaging: "staging-runner"}}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	service := NewReleases(p, memory, &fakeWorkflowController{})
	intent := planner.IntentFromLegacy(applicationRequest())
	intent.TenantID = "tenant-a"
	plan, err := service.PlanIntentForTenant("tenant-a", intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetPlan("tenant-b", plan.ID); code(err) != domain.ReasonTenantBoundary {
		t.Fatalf("cross-tenant plan read error=%v", err)
	}
	run, _, err := service.StartIntentForTenant(context.Background(), "tenant-a", intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetForTenant("tenant-b", run.ID); code(err) != domain.ReasonTenantBoundary {
		t.Fatalf("cross-tenant run read error=%v", err)
	}
	if _, _, err := service.StartIntentForTenant(context.Background(), "tenant-b", intent); code(err) != domain.ReasonTenantBoundary {
		t.Fatalf("tenant substitution error=%v", err)
	}
}

func TestListApprovalsAndRunnerFreezeStayInsideApplicationBoundary(t *testing.T) {
	p, _ := planner.New([]domain.Service{{Name: "identity", RunnerGroups: map[domain.Environment]string{domain.EnvironmentStaging: "staging-runner"}}})
	memory := store.NewMemory()
	service := NewReleases(p, memory, &fakeWorkflowController{})
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "pending", WorkflowID: "pending", RequestID: "pending-request", TenantID: "tenant-a", Plan: domain.ReleasePlan{Hash: "plan"}, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "approval", Status: domain.ApprovalPending, PlanHash: "plan", ExpiresAt: now.Add(time.Hour)}}}}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	approvals, err := service.ListPendingApprovals("tenant-a")
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals=%#v err=%v", approvals, err)
	}
	if other, err := service.ListPendingApprovals("tenant-b"); err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant approvals=%#v err=%v", other, err)
	}
	runner, err := service.FreezeRunner("tenant-a", "staging-runner", "operator")
	if err != nil || runner.Status != domain.RunnerFrozen || runner.Capacity != 0 {
		t.Fatalf("runner=%#v err=%v", runner, err)
	}
	otherTenant := service.ListRunners("tenant-b")
	if len(otherTenant) != 1 || otherTenant[0].Status == domain.RunnerFrozen || otherTenant[0].FrozenBy != "" {
		t.Fatalf("runner freeze crossed tenant boundary: %#v", otherTenant)
	}
}

func TestRunnerHeartbeatCapacityDrainFreezeAndActivate(t *testing.T) {
	p, _ := planner.New([]domain.Service{{Name: "identity", RunnerGroups: map[domain.Environment]string{domain.EnvironmentStaging: "staging-runner"}}})
	memory := store.NewMemory()
	service := NewReleases(p, memory, &fakeWorkflowController{})
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	audit := domain.AuditEvent{ID: "heartbeat", CorrelationID: "runner/tenant-a/runner-1", ActorType: "runner", ActorID: "runner-1", Action: "runner.heartbeat", ResourceType: "runner", ResourceID: "runner-1", Result: "CONNECTED", Timestamp: now}
	if _, err := memory.HeartbeatRunner(context.Background(), domain.RunnerInfo{ID: "runner-1", TenantID: "tenant-a", Group: "staging-runner", ReportedCapacity: 4, InFlight: 1, LastSeen: now}, audit); err != nil {
		t.Fatal(err)
	}
	if capacity := service.RunnerCapacity("tenant-a", "staging-runner"); capacity != 3 {
		t.Fatalf("capacity=%d", capacity)
	}
	draining, err := service.DrainRunner("tenant-a", "runner-1", "operator")
	if err != nil || draining.Status != domain.RunnerDraining || draining.Capacity != 0 {
		t.Fatalf("draining=%#v err=%v", draining, err)
	}
	frozen, err := service.FreezeRunner("tenant-a", "runner-1", "operator")
	if err != nil || frozen.Status != domain.RunnerFrozen || frozen.Capacity != 0 {
		t.Fatalf("frozen=%#v err=%v", frozen, err)
	}
	active, err := service.ActivateRunner("tenant-a", "runner-1", "operator")
	if err != nil || active.Status != domain.RunnerReady || active.Capacity != 3 {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	if _, err := service.FreezeRunner("tenant-b", "runner-1", "operator"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant control error=%v", err)
	}
}

func code(err error) domain.ReasonCode {
	value, _ := domain.ReasonOf(err)
	return value
}

func applicationRequest() domain.ReleaseRequest {
	return domain.ReleaseRequest{RequestID: "request-application", ReleaseVersion: "release-1", Environment: domain.EnvironmentStaging, RequestedBy: "user", Agent: domain.AgentIdentity{ID: "agent", Scopes: []string{"release:deploy"}}, Changes: []domain.Change{{Service: "identity", DesiredVersion: "sha-1", CISuccess: true, DependenciesHealthy: true}}}
}

type fakeWorkflowController struct {
	starts    int
	err       error
	approvals int
}

func (f *fakeWorkflowController) StartRelease(context.Context, workflowcore.ReleaseInput) (workflowcore.Execution, error) {
	f.starts++
	return workflowcore.Execution{WorkflowID: "workflow", RunID: "temporal-run"}, f.err
}

func (f *fakeWorkflowController) SignalApproval(context.Context, string, workflowcore.ApprovalSignal) error {
	f.approvals++
	return nil
}
func (*fakeWorkflowController) Pause(context.Context, string, workflowcore.ControlSignal) error {
	return nil
}
func (*fakeWorkflowController) Resume(context.Context, string, workflowcore.ControlSignal) error {
	return nil
}
func (*fakeWorkflowController) Cancel(context.Context, string, workflowcore.ControlSignal) error {
	return nil
}
