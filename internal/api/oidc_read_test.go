package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/identity"
	"themisy/internal/store"
)

type oidcReadSpy struct {
	OIDCReadModel
	tenants []string
	err     error
}

type mixedTenantReadModel struct{ OIDCReadModel }

func (m mixedTenantReadModel) ListPendingApprovals(string) ([]domain.ApprovalSummary, error) {
	return []domain.ApprovalSummary{{TenantID: "tenant-a"}, {TenantID: "tenant-b", RunID: "cross-tenant-canary"}}, nil
}

func (m mixedTenantReadModel) ListRunners(string) []domain.RunnerInfo {
	return []domain.RunnerInfo{{TenantID: "tenant-a"}, {TenantID: "tenant-b", ID: "cross-tenant-canary"}}
}

func TestOIDCReadAPIRefusesUnexpectedMixedTenantCollections(t *testing.T) {
	app, memory, _ := testApplication(t)
	config, key, claims := principalFixture(t)
	api, err := NewOIDCReadAPI(mixedTenantReadModel{app}, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, memory)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + principalToken(t, key, claims, "idp-key")}
	for _, path := range []string{"/v1/approvals", "/v1/runners"} {
		response := request(t, api.Handler(), http.MethodGet, path, nil, headers)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "cross-tenant-canary") {
			t.Fatal("mixed-tenant model result escaped")
		}
	}
}

func (s *oidcReadSpy) GetPlan(tenant, id string) (domain.ReleasePlan, error) {
	s.tenants = append(s.tenants, tenant)
	if s.err != nil {
		return domain.ReleasePlan{}, s.err
	}
	return s.OIDCReadModel.GetPlan(tenant, id)
}
func (s *oidcReadSpy) GetForTenant(tenant, id string) (*domain.ReleaseRun, error) {
	s.tenants = append(s.tenants, tenant)
	if s.err != nil {
		return nil, s.err
	}
	return s.OIDCReadModel.GetForTenant(tenant, id)
}
func (s *oidcReadSpy) EventsForTenant(tenant, id string) ([]domain.AuditEvent, error) {
	s.tenants = append(s.tenants, tenant)
	if s.err != nil {
		return nil, s.err
	}
	return s.OIDCReadModel.EventsForTenant(tenant, id)
}
func (s *oidcReadSpy) ListPendingApprovals(tenant string) ([]domain.ApprovalSummary, error) {
	s.tenants = append(s.tenants, tenant)
	if s.err != nil {
		return nil, s.err
	}
	return s.OIDCReadModel.ListPendingApprovals(tenant)
}
func (s *oidcReadSpy) ListRunners(tenant string) []domain.RunnerInfo {
	s.tenants = append(s.tenants, tenant)
	return s.OIDCReadModel.ListRunners(tenant)
}

func seedOIDCReads(t *testing.T, memory store.DurableStore) {
	t.Helper()
	now := time.Now().UTC()
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		plan := domain.ReleasePlan{ID: "plan-" + tenant, Hash: "hash-" + tenant, Environment: domain.EnvironmentStaging, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Phases: []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: "identity", Scheduling: domain.SchedulingContext{TenantID: tenant}}}}}}
		if err := memory.SavePlan(plan); err != nil {
			t.Fatal(err)
		}
		run := &domain.ReleaseRun{ID: "run-" + tenant, RequestID: "request-" + tenant, TenantID: tenant, UserIdentityProof: "raw-proof-canary", DelegationRef: "private-delegation-canary", Plan: plan, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Status: domain.StepWaitingApproval, Failure: "private-provider-error-canary", Approval: &domain.Approval{ID: "approval-" + tenant, Status: domain.ApprovalPending, PlanHash: plan.Hash, ExpiresAt: now.Add(time.Hour)}}}}
		if _, _, err := memory.CreateRun(run); err != nil {
			t.Fatal(err)
		}
		if err := memory.AppendAudit(domain.AuditEvent{ID: "event-" + tenant, CorrelationID: run.ID, Action: "run.observed", Result: "UNKNOWN", Details: map[string]any{"raw": "private-detail-canary"}, Timestamp: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := memory.HeartbeatRunner(context.Background(), domain.RunnerInfo{ID: "runner-" + tenant, TenantID: tenant, Group: "group", LastSeen: now}, domain.AuditEvent{ID: "heartbeat-" + tenant, CorrelationID: "fleet-" + tenant, Timestamp: now}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOIDCReadAPIBindsAllReadsAndUsesSummaryProjections(t *testing.T) {
	app, memory, controller := testApplication(t)
	seedOIDCReads(t, memory)
	spy := &oidcReadSpy{OIDCReadModel: app}
	config, key, claims := principalFixture(t)
	sink := &identityAuditRecorder{append: memory.AppendAuditContext}
	api, err := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	token := principalToken(t, key, claims, "idp-key")
	for _, path := range []string{"/v1/plans/plan-tenant-a", "/v1/release-runs/run-tenant-a", "/v1/release-runs/run-tenant-a/events", "/v1/approvals", "/v1/runners"} {
		before := len(sink.events)
		response := request(t, api.Handler(), http.MethodGet, path+"?tenant_id=tenant-b", []byte(`{"tenant_id":"tenant-b"}`), map[string]string{"Authorization": "Bearer " + token, "X-Actor-ID": "forged", "X-Actor-Roles": "admin"})
		if response.Code != http.StatusOK || len(sink.events) != before+1 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		for _, secret := range []string{"tenant-b", token, "raw-proof-canary", "private-delegation-canary", "private-provider-error-canary", "private-detail-canary"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s exposed %s", path, secret)
			}
		}
	}
	if len(spy.tenants) != 5 {
		t.Fatalf("read calls=%d", len(spy.tenants))
	}
	for _, tenant := range spy.tenants {
		if tenant != "tenant-a" {
			t.Fatal("caller chose read tenant")
		}
	}
	if controller.starts != 0 || controller.pauses != 0 || controller.lastApproval.Actor != "" {
		t.Fatal("read invoked a command")
	}
	raw, _ := json.Marshal(sink.events)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "forged") {
		t.Fatal("proof/header leaked to audit")
	}
	run, _ := memory.GetRun("run-tenant-a")
	if run.UserIdentityProof != "raw-proof-canary" || run.StateVersion != 1 {
		t.Fatal("projection mutated durable state")
	}
}

func TestOIDCReadAPIRejectsIdentityRoleAndAuditFailureBeforeReads(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-tenant", "header-tenant", "duplicate-tenant", "unmapped-role", "wrong-audience", "expired", "bad-signature", "removed-key", "audit-failed", "audit-response-lost", "cancelled", "expires-during-audit"} {
		t.Run(kind, func(t *testing.T) {
			app, memory, _ := testApplication(t)
			spy := &oidcReadSpy{OIDCReadModel: app}
			config, key, claims := principalFixture(t)
			now := config.Verifier.Now()
			config.Verifier.Now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := &identityAuditRecorder{append: memory.AppendAuditContext}
			switch kind {
			case "wrong-tenant":
				claims["organization"] = "tenant-b"
			case "unmapped-role":
				claims["groups"] = []string{"unknown"}
			case "wrong-audience":
				claims["aud"] = "other"
			case "expired":
				claims["exp"] = now.Add(-time.Minute).Unix()
			case "audit-failed":
				sink.append = func(context.Context, domain.AuditEvent) error { return errors.New("driver-secret-canary") }
			case "audit-response-lost":
				sink.append = func(ctx context.Context, e domain.AuditEvent) error {
					_ = memory.AppendAuditContext(ctx, e)
					return errors.New("driver-secret-canary")
				}
			case "cancelled":
				cancel()
			case "expires-during-audit":
				sink.append = func(ctx context.Context, e domain.AuditEvent) error {
					now = now.Add(2 * time.Hour)
					return memory.AppendAuditContext(ctx, e)
				}
			}
			token := principalToken(t, key, claims, "idp-key")
			if kind == "bad-signature" {
				token += "invalid"
			}
			api, err := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, sink)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "removed-key" {
				delete(config.Verifier.Keys.(identity.StaticOIDCKeys), config.Verifier.Issuer+"\x00idp-key")
			}
			for _, path := range []string{"/v1/plans/plan-tenant-a", "/v1/release-runs/run-tenant-a", "/v1/release-runs/run-tenant-a/events", "/v1/approvals", "/v1/runners"} {
				r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
				if kind != "missing" {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				r.Header.Set("X-Actor-Roles", "sre")
				if kind == "header-tenant" {
					r.Header.Set("X-Tenant-ID", "tenant-b")
				}
				if kind == "duplicate-tenant" {
					r.Header.Add("X-Tenant-ID", "tenant-a")
					r.Header.Add("X-Tenant-ID", "tenant-a")
				}
				response := httptest.NewRecorder()
				api.Handler().ServeHTTP(response, r)
				want := http.StatusUnauthorized
				if kind == "unmapped-role" {
					want = http.StatusForbidden
				}
				if response.Code != want || len(spy.tenants) != 0 || strings.Contains(response.Body.String(), "canary") {
					t.Fatalf("%s status=%d reads=%v body=%s", path, response.Code, spy.tenants, response.Body.String())
				}
			}
		})
	}
}

func TestOIDCReadAPIDoesNotRegisterCommandsOrGlobalServices(t *testing.T) {
	app, memory, controller := testApplication(t)
	spy := &oidcReadSpy{OIDCReadModel: app}
	config, key, claims := principalFixture(t)
	api, _ := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, memory)
	headers := map[string]string{"Authorization": "Bearer " + principalToken(t, key, claims, "idp-key")}
	for _, path := range []string{"/v1/plans", "/v1/release-runs", "/v1/release-runs:plan", "/v1/release-runs/r:pause", "/v1/release-runs/r/pause", "/v1/release-runs/r/approvals/a/approve", "/v1/approvals/a:approve", "/v1/approvals/a:deny", "/v1/approvals/a:revoke", "/v1/runners/r:freeze", "/v1/runners/r:activate", "/v1/contracts:validate"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			response := request(t, api.Handler(), method, path, releaseBody("tenant-a"), headers)
			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("%s %s: %d", method, path, response.Code)
			}
		}
	}
	if response := request(t, api.Handler(), http.MethodGet, "/v1/services", nil, headers); response.Code != http.StatusNotFound {
		t.Fatal("global services route registered")
	}
	if len(spy.tenants) != 0 || controller.starts != 0 || controller.pauses != 0 || controller.lastApproval.Actor != "" {
		t.Fatal("unsupported route called model/command")
	}
	if response := request(t, api.Handler(), http.MethodGet, "/healthz", nil, nil); response.Code != http.StatusOK {
		t.Fatal("health needs identity")
	}
}

func TestOIDCReadAPIHidesCrossTenantAndStorageErrors(t *testing.T) {
	app, memory, _ := testApplication(t)
	seedOIDCReads(t, memory)
	spy := &oidcReadSpy{OIDCReadModel: app}
	config, key, claims := principalFixture(t)
	api, _ := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, memory)
	headers := map[string]string{"Authorization": "Bearer " + principalToken(t, key, claims, "idp-key")}
	for _, path := range []string{"/v1/plans/plan-tenant-b", "/v1/plans/missing", "/v1/release-runs/run-tenant-b", "/v1/release-runs/missing", "/v1/release-runs/run-tenant-b/events"} {
		response := request(t, api.Handler(), http.MethodGet, path, nil, headers)
		if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":\"OIDC_READ_UNAVAILABLE\"}\n" {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
	}
	spy.err = errors.New("private-store-error-canary")
	for _, path := range []string{"/v1/plans/plan-tenant-a", "/v1/release-runs/run-tenant-a", "/v1/release-runs/run-tenant-a/events", "/v1/approvals"} {
		response := request(t, api.Handler(), http.MethodGet, path, nil, headers)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "canary") {
			t.Fatal("store error escaped")
		}
	}
}

func TestOIDCReadAPIRequiresExplicitComposition(t *testing.T) {
	app, memory, _ := testApplication(t)
	config, _, _ := principalFixture(t)
	if api, err := NewOIDCReadAPI(nil, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, memory); err == nil || api != nil {
		t.Fatal("nil read model accepted")
	}
	for _, role := range []string{"", " sre"} {
		if api, err := NewOIDCReadAPI(app, OIDCReadConfig{Identity: config, RequiredRole: role}, memory); err == nil || api != nil {
			t.Fatal("implicit role accepted")
		}
	}
	if api, err := NewOIDCReadAPI(app, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, nil); err == nil || api != nil {
		t.Fatal("missing audit accepted")
	}
}

func TestOIDCReadAPIReresolvesJWKSBeforeEveryRead(t *testing.T) {
	app, memory, _ := testApplication(t)
	seedOIDCReads(t, memory)
	spy := &oidcReadSpy{OIDCReadModel: app}
	config, key, claims := principalFixture(t)
	now := config.Verifier.Now()
	config.Verifier.Now = func() time.Time { return now }
	issuer := ""
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/discovery" {
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
			return
		}
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"kid": "idp-key", "kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}}})
	}))
	defer server.Close()
	issuer = server.URL
	config.Verifier.Issuer, claims["iss"] = issuer, issuer
	resolver, err := identity.NewOIDCJWKSResolver(map[string]identity.OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/discovery", CacheTTL: time.Minute, HTTPClient: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	resolver.Now = func() time.Time { return now }
	config.Verifier.Keys = resolver
	api, err := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, memory)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + principalToken(t, key, claims, "idp-key")}
	if response := request(t, api.Handler(), http.MethodGet, "/v1/plans/plan-tenant-a", nil, headers); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	now = now.Add(time.Minute + time.Second)
	fail.Store(true)
	for _, path := range []string{"/v1/plans/plan-tenant-a", "/v1/release-runs/run-tenant-a", "/v1/release-runs/run-tenant-a/events", "/v1/approvals", "/v1/runners"} {
		response := request(t, api.Handler(), http.MethodGet, path, nil, headers)
		if response.Code != http.StatusUnauthorized || len(spy.tenants) != 1 {
			t.Fatal("expired JWKS reused a previously admitted read")
		}
	}
}
