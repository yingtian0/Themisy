package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
)

func TestApprovalShowsPinnedContextAndExplicitUnknowns(t *testing.T) {
	app, _, memory := uiApplicationWithStore(t)
	run, _ := memory.GetRun("approval-run")
	run.Steps[0].Verification = &domain.Verification{Status: domain.VerificationMissing, Evidence: domain.Evidence{Source: "synthetic-observer"}}
	if err := memory.UpdateRun(run, run.StateVersion); err != nil {
		t.Fatal(err)
	}
	server, err := New(app, &fixedVerifier{identity: Identity{Subject: "reviewer", TenantID: "tenant-a", Roles: []string{"sre"}}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	for _, want := range []string{"staging / deploy", "requester", "delegated-agent", "requested-artifact", "HIGH_BLAST_RADIUS", "Risk: high", "Runtime verification required", "5m", "synthetic-observer", "MISSING", "not recorded", "Freshness is unknown", "pinned-context", "contract-hash", "profile-hash", "Destination and compensation deadline: unknown", "declared-team", "name=\"plan_hash\" value=\"plan\""} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestApprovalExpiredOrChangedSnapshotCannotSubmit(t *testing.T) {
	for _, scenario := range []string{"expired-approval", "expired-plan", "missing-step", "changed-plan", "changed-artifact", "stale-form", "roles"} {
		t.Run(scenario, func(t *testing.T) {
			app, controller, memory := uiApplicationWithStore(t)
			verifier := &fixedVerifier{identity: Identity{Subject: "reviewer", TenantID: "tenant-a", Roles: []string{"sre"}}}
			server, err := New(app, verifier)
			if err != nil {
				t.Fatal(err)
			}
			before := httptest.NewRecorder()
			server.Handler().ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
			cookie := before.Result().Cookies()[0]
			run, _ := memory.GetRun("approval-run")
			switch scenario {
			case "expired-approval":
				run.Steps[0].Approval.ExpiresAt = time.Now().Add(-time.Second)
			case "expired-plan":
				run.Plan.ExpiresAt = time.Now().Add(-time.Second)
			case "missing-step":
				run.Plan.Phases = nil
			case "changed-plan":
				run.Plan.Hash = "other-plan"
			case "changed-artifact":
				run.Plan.Phases[0].Steps[0].DesiredVersion = "unapproved-artifact"
			case "stale-form":
				run.Plan.Hash = "new-plan"
				run.Steps[0].Approval.PlanHash = "new-plan"
			case "roles":
				verifier.identity.Roles = nil
			}
			if err := memory.UpdateRun(run, run.StateVersion); err != nil {
				t.Fatal(err)
			}
			page := httptest.NewRecorder()
			server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
			if scenario != "stale-form" && scenario != "roles" && !strings.Contains(page.Body.String(), "button disabled") {
				t.Fatal("unsafe approval control remained enabled")
			}
			if scenario == "changed-artifact" && strings.Contains(page.Body.String(), "unapproved-artifact") {
				t.Fatal("unapproved content mixed into pinned review")
			}
			form := url.Values{"csrf_token": {cookie.Value}, "plan_hash": {"plan"}}
			request := httptest.NewRequest(http.MethodPost, "/ui/approvals/approval-1:approve", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusConflict && response.Code != http.StatusForbidden {
				t.Fatalf("unsafe submission status=%d", response.Code)
			}
			if controller.approvals != 0 {
				t.Fatal("unsafe form reached Workflow approval")
			}
		})
	}
}

func TestApprovalContextDoesNotLeakAcrossTenantOrRenderHTML(t *testing.T) {
	app, _, memory := uiApplicationWithStore(t)
	run, _ := memory.GetRun("approval-run")
	run.Plan.Phases[0].Steps[0].DesiredVersion = "<script>alert(1)</script>"
	run.Steps[0].Change.DesiredVersion = run.Plan.Phases[0].Steps[0].DesiredVersion
	if err := memory.UpdateRun(run, run.StateVersion); err != nil {
		t.Fatal(err)
	}
	verifier := &fixedVerifier{identity: Identity{Subject: "viewer", TenantID: "tenant-a"}}
	server, err := New(app, verifier)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
	if strings.Contains(response.Body.String(), "<script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;") {
		t.Fatal("context was not HTML-escaped")
	}
	verifier.identity.TenantID = "tenant-b"
	other := httptest.NewRecorder()
	server.Handler().ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/ui/approvals", nil))
	if strings.Contains(other.Body.String(), "requested-artifact") || strings.Contains(other.Body.String(), "approval-1") || strings.Contains(other.Body.String(), "&lt;script&gt;") {
		t.Fatal("other tenant saw approval details")
	}
}
