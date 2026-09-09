package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
)

type Principal struct {
	Subject  string
	TenantID string
	Roles    []string
}

type IdentityResolver interface {
	Resolve(*http.Request) (Principal, error)
}

type HeaderIdentityResolver struct{}

func (HeaderIdentityResolver) Resolve(r *http.Request) (Principal, error) {
	subject := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
	if subject == "" {
		return Principal{}, errors.New("authenticated X-Actor-ID header is required")
	}
	return Principal{Subject: subject, TenantID: tenantFromRequest(r), Roles: splitHeader(r.Header.Get("X-Actor-Roles"))}, nil
}

type Server struct {
	app      application.ControlPlane
	identity IdentityResolver
	logger   *slog.Logger
	mux      *http.ServeMux
}

func New(app application.ControlPlane, logger *slog.Logger) *Server {
	return NewWithIdentity(app, HeaderIdentityResolver{}, logger)
}

func NewWithIdentity(app application.ControlPlane, identity IdentityResolver, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{app: app, identity: identity, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return requestLog(s.logger, s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /v1/services", s.listServices)
	s.mux.HandleFunc("POST /v1/plans", s.planRelease)
	s.mux.HandleFunc("GET /v1/plans/{id}", s.getPlan)
	s.mux.HandleFunc("POST /v1/release-runs", s.startRelease)
	s.mux.HandleFunc("GET /v1/release-runs/{id}", s.getRelease)
	s.mux.HandleFunc("GET /v1/release-runs/{id}/events", s.getEvents)
	s.mux.HandleFunc("POST /v1/release-runs/{operation}", s.controlColonRoute)
	s.mux.HandleFunc("GET /v1/approvals", s.listApprovals)
	s.mux.HandleFunc("POST /v1/approvals/{operation}", s.approvalColonRoute)
	s.mux.HandleFunc("POST /v1/contracts:validate", s.validateContract)
	s.mux.HandleFunc("GET /v1/runners", s.listRunners)
	s.mux.HandleFunc("POST /v1/runners/{operation}", s.runnerColonRoute)

	s.mux.HandleFunc("POST /v1/release-runs:plan", s.deprecated("/v1/plans", s.planRelease))
	s.mux.HandleFunc("POST /v1/release-runs/{id}/{action}", s.legacyControl)
	s.mux.HandleFunc("POST /v1/release-runs/{id}/approvals/{approvalID}/{action}", s.legacyApproval)
}

func (s *Server) listServices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"services": s.app.Services()})
}

func (s *Server) planRelease(w http.ResponseWriter, r *http.Request) {
	intent, ok := decodeIntent(w, r)
	if !ok {
		return
	}
	plan, err := s.app.PlanIntentForTenant(resolveIntentTenant(r, intent), intent)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.app.GetPlan(tenantFromRequest(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) startRelease(w http.ResponseWriter, r *http.Request) {
	intent, ok := decodeIntent(w, r)
	if !ok {
		return
	}
	run, created, err := s.app.StartIntentForTenant(r.Context(), resolveIntentTenant(r, intent), intent)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, run)
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	run, err := s.app.GetForTenant(tenantFromRequest(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.app.EventsForTenant(tenantFromRequest(r), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) controlColonRoute(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitOperation(r.PathValue("operation"), "pause", "resume", "cancel")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.controlRelease(w, r, id, action)
}

func (s *Server) legacyControl(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "pause" && action != "resume" && action != "cancel" {
		http.NotFound(w, r)
		return
	}
	deprecationHeaders(w, "/v1/release-runs/{id}:"+action)
	s.controlRelease(w, r, r.PathValue("id"), action)
}

func (s *Server) controlRelease(w http.ResponseWriter, r *http.Request, id, action string) {
	principal, err := s.identity.Resolve(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
		return
	}
	run, err := s.app.ControlForTenant(r.Context(), principal.TenantID, id, principal.Subject, action)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	approvals, err := s.app.ListPendingApprovals(tenantFromRequest(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": approvals})
}

func (s *Server) approvalColonRoute(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitOperation(r.PathValue("operation"), "approve", "deny", "revoke")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.decideApproval(w, r, "", id, action)
}

func (s *Server) legacyApproval(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "approve" && action != "deny" {
		http.NotFound(w, r)
		return
	}
	deprecationHeaders(w, "/v1/approvals/{id}:"+action)
	s.decideApproval(w, r, r.PathValue("id"), r.PathValue("approvalID"), action)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, runID, approvalID, action string) {
	principal, err := s.identity.Resolve(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
		return
	}
	if runID == "" {
		approvals, listErr := s.app.ListPendingApprovals(principal.TenantID)
		if listErr != nil {
			writeError(w, listErr)
			return
		}
		for _, approval := range approvals {
			if approval.Approval.ID == approvalID {
				runID = approval.RunID
				break
			}
		}
	}
	if runID == "" {
		writeError(w, store.ErrNotFound)
		return
	}
	var run *domain.ReleaseRun
	if action == "revoke" {
		run, err = s.app.RevokeApprovalForTenant(r.Context(), principal.TenantID, runID, approvalID, principal.Subject, principal.Roles)
	} else {
		run, err = s.app.DecideApprovalForTenant(r.Context(), principal.TenantID, runID, approvalID, principal.Subject, principal.Roles, action == "approve")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) validateContract(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	result, err := s.app.ValidateContract(data)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runners": s.app.ListRunners(tenantFromRequest(r))})
}

func (s *Server) runnerColonRoute(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitOperation(r.PathValue("operation"), "freeze", "drain", "activate")
	if !ok {
		http.NotFound(w, r)
		return
	}
	principal, err := s.identity.Resolve(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: err.Error()})
		return
	}
	var runner domain.RunnerInfo
	switch action {
	case "freeze":
		runner, err = s.app.FreezeRunner(principal.TenantID, id, principal.Subject)
	case "drain":
		runner, err = s.app.DrainRunner(principal.TenantID, id, principal.Subject)
	case "activate":
		runner, err = s.app.ActivateRunner(principal.TenantID, id, principal.Subject)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runner)
}

func (s *Server) deprecated(successor string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deprecationHeaders(w, successor)
		next(w, r)
	}
}

func deprecationHeaders(w http.ResponseWriter, successor string) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"successor-version\"", successor))
}

func splitOperation(value string, allowed ...string) (string, string, bool) {
	id, action, ok := strings.Cut(value, ":")
	if !ok || id == "" {
		return "", "", false
	}
	for _, candidate := range allowed {
		if action == candidate {
			return id, action, true
		}
	}
	return "", "", false
}

func tenantFromRequest(r *http.Request) string {
	if tenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); tenant != "" {
		return tenant
	}
	return "default"
}

func resolveIntentTenant(r *http.Request, intent domain.ReleaseIntent) string {
	if tenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); tenant != "" {
		return tenant
	}
	if intent.TenantID != "" {
		return intent.TenantID
	}
	return "default"
}

func splitHeader(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

type errorResponse struct {
	Error      string            `json:"error"`
	ReasonCode domain.ReasonCode `json:"reason_code,omitempty"`
}

func decodeIntent(w http.ResponseWriter, r *http.Request) (domain.ReleaseIntent, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return domain.ReleaseIntent{}, false
	}
	var discriminator struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return domain.ReleaseIntent{}, false
	}
	if discriminator.APIVersion == "" {
		var request domain.ReleaseRequest
		if !decodeStrictJSON(w, data, &request) {
			return domain.ReleaseIntent{}, false
		}
		return planner.IntentFromLegacy(request), true
	}
	var intent domain.ReleaseIntent
	if !decodeStrictJSON(w, data, &intent) {
		return domain.ReleaseIntent{}, false
	}
	return intent, true
}

func decodeStrictJSON(w http.ResponseWriter, data []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: multiple values are not allowed"})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	response := errorResponse{Error: err.Error()}
	reason, hasReason := domain.ReasonOf(err)
	if hasReason {
		response.ReasonCode = reason
	}
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, store.ErrConflict) {
		status = http.StatusConflict
	} else if errors.Is(err, application.ErrApproval) {
		status = http.StatusForbidden
	} else if errors.Is(err, planner.ErrDependencyCycle) {
		status = http.StatusUnprocessableEntity
	} else if hasReason {
		switch reason {
		case domain.ReasonTenantBoundary:
			status = http.StatusForbidden
		case domain.ReasonPlanExpired, domain.ReasonPlanHashMismatch, domain.ReasonContractChanged,
			domain.ReasonProfileChanged, domain.ReasonContextChanged, domain.ReasonRunnerFrozen,
			domain.ReasonCircuitOpen, domain.ReasonBackpressure:
			status = http.StatusConflict
		default:
			status = http.StatusUnprocessableEntity
		}
	}
	writeJSON(w, status, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
