package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"slices"
	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/identity"
)

const ProtocolVersion = "2025-11-25"

type Principal struct {
	Subject  string
	TenantID string
	Roles    []string
}

type PrincipalResolver interface {
	Resolve(*http.Request) (Principal, error)
}

type HeaderPrincipalResolver struct{}

func (HeaderPrincipalResolver) Resolve(r *http.Request) (Principal, error) {
	subject := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
	tenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if tenant == "" {
		return Principal{}, errors.New("authenticated X-Tenant-ID header is required")
	}
	parts := strings.Split(r.Header.Get("X-Actor-Roles"), ",")
	roles := make([]string, 0, len(parts))
	for _, part := range parts {
		if role := strings.TrimSpace(part); role != "" {
			roles = append(roles, role)
		}
	}
	return Principal{Subject: subject, TenantID: tenant, Roles: roles}, nil
}

type Explainer interface {
	Explain(context.Context, domain.ReleasePlan) (string, error)
}

type Server struct {
	app       application.ControlPlane
	principal Principal
	explainer Explainer
	resolver  PrincipalResolver
}

func NewHTTP(app application.ControlPlane, resolver PrincipalResolver, explainer Explainer) *Server {
	return &Server{app: app, resolver: resolver, explainer: explainer}
}

func New(app application.ControlPlane, principal Principal, explainer Explainer) *Server {
	if principal.TenantID == "" {
		principal.TenantID = "default"
	}
	return &Server{app: app, principal: principal, explainer: explainer}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type callResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "MCP uses POST", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "origin is not allowed", http.StatusForbidden)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedProtocol(version) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	active := s
	if s.resolver != nil {
		principal, err := s.resolver.Resolve(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		copy := *s
		copy.principal = principal
		active = &copy
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var message request
	if err := decodeStrict(r.Body, &message); err != nil {
		writeRPC(w, http.StatusBadRequest, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if message.JSONRPC != "2.0" || message.Method == "" {
		writeRPC(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: -32600, Message: "invalid JSON-RPC request"}})
		return
	}
	if mirrored := r.Header.Get("Mcp-Method"); mirrored != "" && mirrored != message.Method {
		writeRPC(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: -32600, Message: "Mcp-Method header does not match request"}})
		return
	}
	if message.Method == "tools/call" && r.Header.Get("Mcp-Name") != "" {
		var params callParams
		if err := decodeRaw(message.Params, &params); err != nil || params.Name != r.Header.Get("Mcp-Name") {
			writeRPC(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: -32600, Message: "Mcp-Name header does not match request"}})
			return
		}
	}
	if len(message.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := active.handle(r.Context(), message)
	writeRPC(w, http.StatusOK, response{JSONRPC: "2.0", ID: message.ID, Result: result, Error: rpcErr})
}

func (s *Server) handle(ctx context.Context, message request) (any, *rpcError) {
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		if err := decodeRaw(message.Params, &params); err != nil {
			return nil, invalidParams(err)
		}
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "themisy", "version": "v1alpha1"}}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var params callParams
		if err := decodeRaw(message.Params, &params); err != nil || params.Name == "" {
			if err == nil {
				err = errors.New("tool name is required")
			}
			return nil, invalidParams(err)
		}
		result, err := s.Call(ctx, params.Name, params.Arguments)
		if err != nil {
			return errorResult(err), nil
		}
		return successResult(result), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) Call(ctx context.Context, name string, arguments json.RawMessage) (map[string]any, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	switch name {
	case "list_services":
		if err := decodeRaw(arguments, &struct{}{}); err != nil {
			return nil, err
		}
		return object("services", s.app.Services())
	case "get_service_context":
		var input serviceArgument
		if err := decodeRequired(arguments, &input, func() bool { return input.Service != "" }); err != nil {
			return nil, err
		}
		for _, service := range s.app.Services() {
			if service.Name == input.Service {
				return object("service", service)
			}
		}
		return nil, fmt.Errorf("service %q not found", input.Service)
	case "plan_release":
		intent, err := decodeIntentArgument(arguments)
		if err != nil {
			return nil, err
		}
		if err := identity.BindIntentActor(ctx, &intent); err != nil {
			return nil, err
		}
		var plan domain.ReleasePlan
		if contextual, ok := s.app.(interface {
			PlanIntentContext(context.Context, string, domain.ReleaseIntent) (domain.ReleasePlan, error)
		}); ok {
			plan, err = contextual.PlanIntentContext(ctx, s.principal.TenantID, intent)
		} else {
			plan, err = s.app.PlanIntentForTenant(s.principal.TenantID, intent)
		}
		if err != nil {
			return nil, err
		}
		return object("plan", plan)
	case "explain_release_plan":
		var input planArgument
		if err := decodeRequired(arguments, &input, func() bool { return input.PlanID != "" }); err != nil {
			return nil, err
		}
		plan, err := s.app.GetPlan(s.principal.TenantID, input.PlanID)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"plan": plan, "system_reasons": planReasons(plan), "ai_explanation": nil}
		if s.explainer != nil {
			explanation, explainErr := s.explainer.Explain(ctx, plan)
			if explainErr != nil {
				return nil, explainErr
			}
			result["ai_explanation"] = explanation
		}
		return result, nil
	case "start_release":
		intent, err := decodeIntentArgument(arguments)
		if err != nil {
			return nil, err
		}
		if err := identity.BindIntentActor(ctx, &intent); err != nil {
			return nil, err
		}
		run, created, err := s.app.StartIntentForTenant(ctx, s.principal.TenantID, intent)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run": publicRun(run), "created": created}, nil
	case "get_release_status":
		return s.runResult(arguments, func(runID string) (any, error) { return s.app.GetForTenant(s.principal.TenantID, runID) })
	case "pause_release":
		if !operatorAllowed(ctx) {
			return nil, fmt.Errorf("operator role required")
		}
		return s.runResult(arguments, func(runID string) (any, error) {
			return s.app.ControlForTenant(ctx, s.principal.TenantID, runID, s.principal.Subject, "pause")
		})
	case "cancel_release":
		if !operatorAllowed(ctx) {
			return nil, fmt.Errorf("operator role required")
		}
		return s.runResult(arguments, func(runID string) (any, error) {
			return s.app.ControlForTenant(ctx, s.principal.TenantID, runID, s.principal.Subject, "cancel")
		})
	case "list_pending_approvals":
		if err := decodeRaw(arguments, &struct{}{}); err != nil {
			return nil, err
		}
		approvals, err := s.app.ListPendingApprovals(s.principal.TenantID)
		if err != nil {
			return nil, err
		}
		return object("approvals", approvals)
	case "get_incident_context":
		var input runArgument
		if err := decodeRequired(arguments, &input, func() bool { return input.RunID != "" }); err != nil {
			return nil, err
		}
		run, err := s.app.GetForTenant(s.principal.TenantID, input.RunID)
		if err != nil {
			return nil, err
		}
		events, err := s.app.EventsForTenant(s.principal.TenantID, input.RunID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run": publicRun(run), "events": events}, nil
	default:
		return nil, fmt.Errorf("unknown or prohibited tool %q", name)
	}
}

func (s *Server) runResult(arguments json.RawMessage, operation func(string) (any, error)) (map[string]any, error) {
	var input runArgument
	if err := decodeRequired(arguments, &input, func() bool { return input.RunID != "" }); err != nil {
		return nil, err
	}
	result, err := operation(input.RunID)
	if err != nil {
		return nil, err
	}
	if run, ok := result.(*domain.ReleaseRun); ok {
		result = publicRun(run)
	}
	return object("run", result)
}

func decodeIntentArgument(arguments json.RawMessage) (domain.ReleaseIntent, error) {
	var input intentArgument
	if err := decodeRequired(arguments, &input, func() bool { return len(input.Intent) > 0 }); err != nil {
		return domain.ReleaseIntent{}, err
	}
	var intent domain.ReleaseIntent
	if err := decodeRaw(input.Intent, &intent); err != nil {
		return domain.ReleaseIntent{}, fmt.Errorf("invalid structured intent: %w", err)
	}
	return intent, nil
}

func decodeRequired(data json.RawMessage, target any, valid func() bool) error {
	if err := decodeRaw(data, target); err != nil {
		return err
	}
	if !valid() {
		return errors.New("required structured argument is missing")
	}
	return nil
}

func decodeRaw(data json.RawMessage, target any) error {
	return decodeStrict(bytes.NewReader(data), target)
}

func decodeStrict(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func object(key string, value any) (map[string]any, error) { return map[string]any{key: value}, nil }

func planReasons(plan domain.ReleasePlan) []map[string]string {
	reasons := []map[string]string{{"code": "PLAN_HASH_PINNED", "source": "system"}}
	for _, phase := range plan.Phases {
		for _, step := range phase.Steps {
			if len(step.Dependencies) > 0 {
				reasons = append(reasons, map[string]string{"code": "DEPENDENCY_ORDER", "service": step.Service, "source": "system"})
			}
			if step.VerificationRequired {
				reasons = append(reasons, map[string]string{"code": "VERIFICATION_REQUIRED", "service": step.Service, "source": "system"})
			}
		}
	}
	return reasons
}

func successResult(value map[string]any) callResult {
	data, _ := json.Marshal(value)
	return callResult{Content: []textContent{{Type: "text", Text: string(data)}}, StructuredContent: value}
}

func errorResult(err error) callResult {
	value := map[string]any{"error": err.Error()}
	if reason, ok := domain.ReasonOf(err); ok {
		value["reason_code"] = reason
	}
	data, _ := json.Marshal(value)
	return callResult{Content: []textContent{{Type: "text", Text: string(data)}}, StructuredContent: value, IsError: true}
}

func invalidParams(err error) *rpcError {
	return &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
}

func writeRPC(w http.ResponseWriter, status int, message response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(message)
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

func supportedProtocol(version string) bool {
	switch version {
	case "2025-03-26", "2025-06-18", ProtocolVersion:
		return true
	default:
		return false
	}
}

func publicRun(run *domain.ReleaseRun) *domain.ReleaseRun {
	if run == nil {
		return nil
	}
	copy := *run
	copy.UserIdentityProof = ""
	return &copy
}
func operatorAllowed(ctx context.Context) bool {
	actor, ok := identity.RequestActorFromContext(ctx)
	return (!ok && !identity.HasRequestActor(ctx)) || slices.Contains(actor.Roles, "operator") || slices.Contains(actor.Roles, "admin") || slices.Contains(actor.Roles, "sre")
}
