package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"

	"go.yaml.in/yaml/v3"
)

func TestPlanGetStartAndLegacyDeprecation(t *testing.T) {
	service, _, _ := testApplication(t)
	handler := New(service, testLogger()).Handler()
	body := releaseBody("tenant-a")

	planResponse := request(t, handler, http.MethodPost, "/v1/plans", body, map[string]string{"X-Tenant-ID": "tenant-a"})
	if planResponse.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResponse.Code, planResponse.Body.String())
	}
	var plan domain.ReleasePlan
	decodeResponse(t, planResponse, &plan)
	if plan.Hash == "" || plan.ID == "" {
		t.Fatalf("plan=%#v", plan)
	}

	getResponse := request(t, handler, http.MethodGet, "/v1/plans/"+plan.ID, nil, map[string]string{"X-Tenant-ID": "tenant-a"})
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get plan status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	var fetched domain.ReleasePlan
	decodeResponse(t, getResponse, &fetched)
	if fetched.Hash != plan.Hash {
		t.Fatalf("fetched hash=%s want=%s", fetched.Hash, plan.Hash)
	}

	startResponse := request(t, handler, http.MethodPost, "/v1/release-runs", body, map[string]string{"X-Tenant-ID": "tenant-a"})
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	var run domain.ReleaseRun
	decodeResponse(t, startResponse, &run)
	if run.Status != domain.RunPending || run.TenantID != "tenant-a" {
		t.Fatalf("run=%#v", run)
	}

	legacy := request(t, handler, http.MethodPost, "/v1/release-runs:plan", body, nil)
	if legacy.Code != http.StatusOK || legacy.Header().Get("Deprecation") != "true" || legacy.Header().Get("Link") == "" {
		t.Fatalf("legacy status=%d headers=%v body=%s", legacy.Code, legacy.Header(), legacy.Body.String())
	}
}

func TestTenantBoundaryAndColonControls(t *testing.T) {
	service, _, controller := testApplication(t)
	handler := New(service, testLogger()).Handler()
	started := request(t, handler, http.MethodPost, "/v1/release-runs", releaseBody("tenant-a"), map[string]string{"X-Tenant-ID": "tenant-a"})
	var run domain.ReleaseRun
	decodeResponse(t, started, &run)

	crossTenant := request(t, handler, http.MethodGet, "/v1/release-runs/"+run.ID, nil, map[string]string{"X-Tenant-ID": "tenant-b"})
	if crossTenant.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant status=%d body=%s", crossTenant.Code, crossTenant.Body.String())
	}
	paused := request(t, handler, http.MethodPost, "/v1/release-runs/"+run.ID+":pause", nil, map[string]string{"X-Tenant-ID": "tenant-a", "X-Actor-ID": "operator"})
	if paused.Code != http.StatusOK || controller.pauses != 1 {
		t.Fatalf("pause status=%d calls=%d body=%s", paused.Code, controller.pauses, paused.Body.String())
	}
}

func TestRunnerDrainFreezeAndActivateControls(t *testing.T) {
	service, memory, _ := testApplication(t)
	now := time.Now().UTC()
	audit := domain.AuditEvent{ID: "runner-connect", CorrelationID: "runner/tenant-a/runner-1", ActorType: "runner", ActorID: "runner-1", Action: "runner.connect", ResourceType: "runner", ResourceID: "runner-1", Result: "CONNECTED", Timestamp: now}
	if _, err := memory.HeartbeatRunner(context.Background(), domain.RunnerInfo{ID: "runner-1", TenantID: "tenant-a", Group: "staging-runner", ReportedCapacity: 2, LastSeen: now}, audit); err != nil {
		t.Fatal(err)
	}
	handler := New(service, testLogger()).Handler()
	for _, test := range []struct {
		action string
		status domain.RunnerStatus
	}{{"drain", domain.RunnerDraining}, {"freeze", domain.RunnerFrozen}, {"activate", domain.RunnerReady}} {
		response := request(t, handler, http.MethodPost, "/v1/runners/runner-1:"+test.action, nil, map[string]string{"X-Tenant-ID": "tenant-a", "X-Actor-ID": "operator"})
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", test.action, response.Code, response.Body.String())
		}
		var runner domain.RunnerInfo
		decodeResponse(t, response, &runner)
		if runner.Status != test.status {
			t.Fatalf("%s runner=%#v", test.action, runner)
		}
	}
}

func TestApprovalIdentityIsRevalidatedServerSide(t *testing.T) {
	service, memory, controller := testApplication(t)
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "approval-run", WorkflowID: "approval-run", RequestID: "approval-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "approval-1", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	resolver := &fixedIdentity{principal: Principal{Subject: "verified-sre", TenantID: "tenant-a", Roles: []string{"sre"}}}
	handler := NewWithIdentity(service, resolver, testLogger()).Handler()
	response := request(t, handler, http.MethodPost, "/v1/approvals/approval-1:approve", []byte(`{"actor":"attacker","roles":["admin"]}`), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.calls != 1 || controller.lastApproval.Actor != "verified-sre" || len(controller.lastApproval.Roles) != 1 || controller.lastApproval.Roles[0] != "sre" {
		t.Fatalf("resolver calls=%d signal=%#v", resolver.calls, controller.lastApproval)
	}
}

func TestContractValidation(t *testing.T) {
	service, _, _ := testApplication(t)
	handler := New(service, testLogger()).Handler()
	contract, err := os.ReadFile("../../examples/contracts/identity-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost, "/v1/contracts:validate", contract, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var validation domain.ContractValidation
	decodeResponse(t, response, &validation)
	if !validation.Valid || validation.Name != "identity-api" || validation.ContentHash == "" {
		t.Fatalf("validation=%#v", validation)
	}
}

func TestOpenAPIIsParseableAndDocumentsRequiredRoutes(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi/v1alpha1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi=%q", document.OpenAPI)
	}
	for path, method := range map[string]string{
		"/v1/plans": "post", "/v1/plans/{id}": "get", "/v1/release-runs": "post",
		"/v1/release-runs/{id}:pause": "post", "/v1/release-runs/{id}:resume": "post",
		"/v1/release-runs/{id}:cancel": "post", "/v1/approvals": "get",
		"/v1/approvals/{id}:approve": "post", "/v1/approvals/{id}:deny": "post",
		"/v1/approvals/{id}:revoke": "post", "/v1/contracts:validate": "post",
		"/v1/runners": "get", "/v1/runners/{id}:freeze": "post",
		"/v1/runners/{id}:drain": "post", "/v1/runners/{id}:activate": "post",
	} {
		if _, ok := document.Paths[path][method]; !ok {
			t.Errorf("OpenAPI missing %s %s", method, path)
		}
	}
}

type fixedIdentity struct {
	principal Principal
	calls     int
}

func (f *fixedIdentity) Resolve(*http.Request) (Principal, error) {
	f.calls++
	return f.principal, nil
}

type fakeController struct {
	starts       int
	pauses       int
	lastApproval workflowcore.ApprovalSignal
}

func (f *fakeController) StartRelease(context.Context, workflowcore.ReleaseInput) (workflowcore.Execution, error) {
	f.starts++
	return workflowcore.Execution{WorkflowID: "workflow", RunID: "run"}, nil
}
func (f *fakeController) SignalApproval(_ context.Context, _ string, signal workflowcore.ApprovalSignal) error {
	f.lastApproval = signal
	return nil
}
func (f *fakeController) Pause(context.Context, string, workflowcore.ControlSignal) error {
	f.pauses++
	return nil
}
func (*fakeController) Resume(context.Context, string, workflowcore.ControlSignal) error { return nil }
func (*fakeController) Cancel(context.Context, string, workflowcore.ControlSignal) error { return nil }

func testApplication(t *testing.T) (*application.Releases, *store.Memory, *fakeController) {
	t.Helper()
	p, err := planner.New([]domain.Service{{Name: "identity", OwnerTeam: "identity", RiskTier: "high", RunnerGroups: map[domain.Environment]string{domain.EnvironmentStaging: "staging-runner"}}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	controller := &fakeController{}
	return application.NewReleases(p, memory, controller), memory, controller
}

func releaseBody(tenant string) []byte {
	return []byte(`{"api_version":"execution.themisy.io/v1alpha1","kind":"ReleaseIntent","request_id":"http-1","release_version":"release-1","tenant_id":"` + tenant + `","environment":"staging","requested_by":"user-1","delegated_agent":{"id":"agent-1","scopes":["release:deploy"]},"changes":[{"service":"identity","desired_version":"sha-1","ci_success":true,"dependencies_healthy":true}]}`)
}

func request(t *testing.T, handler http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
