package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
)

func TestApprovalUIRevalidatesIdentityAndRejectsCSRF(t *testing.T) {
	app, controller := uiApplication(t)
	verifier := &fixedVerifier{identity: Identity{Subject: "verified-sre", TenantID: "tenant-a", Roles: []string{"sre"}}}
	server, err := New(app, verifier)
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	server.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "approval-1") {
		t.Fatalf("status=%d body=%s", get.Code, get.Body.String())
	}
	csrfCookie := get.Result().Cookies()[0]

	bad := httptest.NewRecorder()
	badRequest := httptest.NewRequest(http.MethodPost, "/ui/approvals/approval-1:approve", strings.NewReader("csrf_token=bad"))
	badRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badRequest.AddCookie(csrfCookie)
	server.Handler().ServeHTTP(bad, badRequest)
	if bad.Code != http.StatusForbidden || controller.approvals != 0 {
		t.Fatalf("bad csrf status=%d approvals=%d", bad.Code, controller.approvals)
	}

	form := url.Values{"csrf_token": {csrfCookie.Value}, "plan_hash": {"plan"}, "actor": {"attacker"}, "roles": {"admin"}}
	approved := httptest.NewRecorder()
	approveRequest := httptest.NewRequest(http.MethodPost, "/ui/approvals/approval-1:approve", strings.NewReader(form.Encode()))
	approveRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	approveRequest.AddCookie(csrfCookie)
	server.Handler().ServeHTTP(approved, approveRequest)
	if approved.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", approved.Code, approved.Body.String())
	}
	if verifier.calls != 3 || controller.last.Actor != "verified-sre" || len(controller.last.Roles) != 1 || controller.last.Roles[0] != "sre" {
		t.Fatalf("verifier calls=%d signal=%#v", verifier.calls, controller.last)
	}
}

func TestRunUIEnforcesTenantBoundary(t *testing.T) {
	app, _ := uiApplication(t)
	verifier := &fixedVerifier{identity: Identity{Subject: "viewer", TenantID: "tenant-b"}}
	server, err := New(app, verifier)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/release-runs/approval-run", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type fixedVerifier struct {
	identity Identity
	calls    int
}

func (f *fixedVerifier) Verify(*http.Request) (Identity, error) {
	f.calls++
	return f.identity, nil
}

type fakeController struct {
	approvals int
	last      workflowcore.ApprovalSignal
}

func (*fakeController) StartRelease(context.Context, workflowcore.ReleaseInput) (workflowcore.Execution, error) {
	return workflowcore.Execution{}, nil
}
func (f *fakeController) SignalApproval(_ context.Context, _ string, signal workflowcore.ApprovalSignal) error {
	f.approvals++
	f.last = signal
	return nil
}
func (*fakeController) Pause(context.Context, string, workflowcore.ControlSignal) error  { return nil }
func (*fakeController) Resume(context.Context, string, workflowcore.ControlSignal) error { return nil }
func (*fakeController) Cancel(context.Context, string, workflowcore.ControlSignal) error { return nil }

func uiApplication(t *testing.T) (*application.Releases, *fakeController) {
	app, controller, _ := uiApplicationWithStore(t)
	return app, controller
}

func uiApplicationWithStore(t *testing.T) (*application.Releases, *fakeController, *store.Memory) {
	t.Helper()
	p, err := planner.New([]domain.Service{{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "approval-run", WorkflowID: "approval-run", RequestID: "approval-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "approval-1", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
	run.Environment = domain.EnvironmentStaging
	run.Agent.ID = "delegated-agent"
	run.Plan.Environment = run.Environment
	run.Plan.ExpiresAt = now.Add(time.Hour)
	run.Plan.ContextSnapshotRef = "pinned-context"
	run.Plan.Phases = []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: "identity", DesiredVersion: "requested-artifact", Profile: "pinned-profile", ContractHash: "contract-hash", ProfileHash: "profile-hash", VerificationRequired: true, ObservationWindow: "5m", RollbackMode: domain.RollbackAutomatic, Scheduling: domain.SchedulingContext{RiskTier: "high", Team: "declared-team"}}}}}
	run.Steps[0].Status = domain.StepWaitingApproval
	run.Steps[0].Change = domain.Change{Service: "identity", DesiredVersion: "requested-artifact"}
	run.Steps[0].Policy = &domain.PolicyDecision{Decision: domain.DecisionRequireApproval, ReasonCode: "HIGH_BLAST_RADIUS", ReasonDetail: "review the declared impact"}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	return application.NewReleases(p, memory, controller), controller, memory
}
