package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"themisy/internal/domain"
)

var ErrOIDCReadConfiguration = errors.New("OIDC_READ_CONFIGURATION_INVALID")

// OIDCReadModel deliberately exposes no command, planning or provider method.
type OIDCReadModel interface {
	GetPlan(string, string) (domain.ReleasePlan, error)
	GetForTenant(string, string) (*domain.ReleaseRun, error)
	EventsForTenant(string, string) ([]domain.AuditEvent, error)
	ListPendingApprovals(string) ([]domain.ApprovalSummary, error)
	ListRunners(string) []domain.RunnerInfo
}

type OIDCReadConfig struct {
	Identity     OIDCPrincipalConfig
	RequiredRole string
}

// OIDCReadAPI is an opt-in, tenant-local summary API. It cannot start a release
// or decide an approval and is not the production composition of the old API.
type OIDCReadAPI struct {
	model    OIDCReadModel
	identity *AuditedOIDCIdentityResolver
	tenant   string
	role     string
	mux      *http.ServeMux
}

func NewOIDCReadAPI(model OIDCReadModel, config OIDCReadConfig, sink IdentityAuditSink) (*OIDCReadAPI, error) {
	if model == nil || config.RequiredRole == "" || strings.TrimSpace(config.RequiredRole) != config.RequiredRole {
		return nil, ErrOIDCReadConfiguration
	}
	resolver, err := NewAuditedOIDCIdentityResolver(config.Identity, sink)
	if err != nil {
		return nil, err
	}
	a := &OIDCReadAPI{model: model, identity: resolver, tenant: config.Identity.TenantID, role: config.RequiredRole, mux: http.NewServeMux()}
	a.mux.HandleFunc("GET /v1/plans/{id}", a.getPlan)
	a.mux.HandleFunc("GET /v1/release-runs/{id}", a.getRun)
	a.mux.HandleFunc("GET /v1/release-runs/{id}/events", a.getEvents)
	a.mux.HandleFunc("GET /v1/approvals", a.getApprovals)
	a.mux.HandleFunc("GET /v1/runners", a.getRunners)
	return a, nil
}

func (a *OIDCReadAPI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Even rejected/unknown routes must not cache protected responses.
		w.Header().Set("Cache-Control", "no-store")
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		principal, err := a.identity.Resolve(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "OIDC_READ_IDENTITY_UNAVAILABLE"})
			return
		}
		allowed := false
		for _, role := range principal.Roles {
			allowed = allowed || role == a.role
		}
		if !allowed || principal.TenantID != a.tenant {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "OIDC_READ_ROLE_REQUIRED"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "OIDC_READ_ONLY_API"})
			return
		}
		a.mux.ServeHTTP(w, r)
	})
}

type oidcPlanSummary struct {
	ID          string             `json:"plan_id"`
	TenantID    string             `json:"tenant_id"`
	Environment domain.Environment `json:"environment"`
	Hash        string             `json:"hash"`
	CreatedAt   time.Time          `json:"created_at"`
	ExpiresAt   time.Time          `json:"expires_at"`
}

func (a *OIDCReadAPI) getPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := a.model.GetPlan(a.tenant, r.PathValue("id"))
	if err != nil {
		a.readError(w)
		return
	}
	writeJSON(w, http.StatusOK, oidcPlanSummary{plan.ID, a.tenant, plan.Environment, plan.Hash, plan.CreatedAt, plan.ExpiresAt})
}

type oidcStepSummary struct {
	Service string            `json:"service"`
	Status  domain.StepStatus `json:"status"`
}

type oidcRunSummary struct {
	ID           string             `json:"id"`
	TenantID     string             `json:"tenant_id"`
	Environment  domain.Environment `json:"environment"`
	Status       domain.RunStatus   `json:"status"`
	PlanHash     string             `json:"plan_hash"`
	StateVersion int64              `json:"state_version"`
	Steps        []oidcStepSummary  `json:"steps"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

func (a *OIDCReadAPI) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.model.GetForTenant(a.tenant, r.PathValue("id"))
	if err != nil || run == nil || run.TenantID != a.tenant {
		a.readError(w)
		return
	}
	steps := make([]oidcStepSummary, 0, len(run.Steps))
	for _, step := range run.Steps {
		steps = append(steps, oidcStepSummary{step.Service, step.Status})
	}
	writeJSON(w, http.StatusOK, oidcRunSummary{run.ID, a.tenant, run.Environment, run.Status, run.Plan.Hash, run.StateVersion, steps, run.CreatedAt, run.UpdatedAt})
}

// Event Details and free-form identity/proof/provider payloads are not part of
// this summary protocol. Adding a field requires an explicit projection review.
type oidcEventSummary struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Result       string    `json:"result"`
	Timestamp    time.Time `json:"timestamp"`
}

func (a *OIDCReadAPI) getEvents(w http.ResponseWriter, r *http.Request) {
	events, err := a.model.EventsForTenant(a.tenant, r.PathValue("id"))
	if err != nil {
		a.readError(w)
		return
	}
	result := make([]oidcEventSummary, 0, len(events))
	for _, event := range events {
		result = append(result, oidcEventSummary{event.ID, event.Action, event.ResourceType, event.ResourceID, event.Result, event.Timestamp})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": result})
}

func (a *OIDCReadAPI) getApprovals(w http.ResponseWriter, _ *http.Request) {
	approvals, err := a.model.ListPendingApprovals(a.tenant)
	if err != nil {
		a.readError(w)
		return
	}
	for _, approval := range approvals {
		if approval.TenantID != a.tenant {
			a.readError(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
}

func (a *OIDCReadAPI) getRunners(w http.ResponseWriter, _ *http.Request) {
	runners := a.model.ListRunners(a.tenant)
	for _, runner := range runners {
		if runner.TenantID != a.tenant {
			a.readError(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

func (a *OIDCReadAPI) readError(w http.ResponseWriter) {
	// Do not reveal another tenant's existence or underlying storage errors.
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "OIDC_READ_UNAVAILABLE"})
}
