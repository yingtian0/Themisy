package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/identity"
)

func principalFixture(t *testing.T) (OIDCPrincipalConfig, ed25519.PrivateKey, map[string]any) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	config := OIDCPrincipalConfig{TenantID: "tenant-a", TenantClaim: "organization", RoleClaim: "groups", RoleMappings: map[string][]string{"ops-team": {"sre", "operator"}, "oncall": {"sre"}}, Verifier: identity.OIDCVerifier{
		Issuer: "https://idp.example.test", Audience: "themisy-tenant-a", Keys: identity.StaticOIDCKeys{"https://idp.example.test\x00idp-key": public}, Now: func() time.Time { return now },
	}}
	return config, private, map[string]any{"iss": config.Verifier.Issuer, "sub": "verified-approver", "aud": config.Verifier.Audience, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(), "organization": "tenant-a", "groups": []string{"ops-team", "oncall", "unmapped"}}
}

func principalToken(t *testing.T, private ed25519.PrivateKey, claims map[string]any, kid string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": kid, "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(input)))
}

func TestOIDCPrincipalUsesVerifiedMappedClaims(t *testing.T) {
	config, private, claims := principalFixture(t)
	resolver, err := NewOIDCIdentityResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	// Configuration is snapshotted; caller mutation cannot add roles later.
	config.RoleMappings["ops-team"][0] = "admin"
	config.Verifier.Issuer = "https://wrong.example.test"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"actor":"forged","roles":["admin"]}`))
	req.Header.Set("Authorization", "Bearer "+principalToken(t, private, claims, "idp-key"))
	req.Header.Set("X-Actor-ID", "forged")
	req.Header.Set("X-Actor-Roles", "admin")
	principal, err := resolver.Resolve(req)
	if err != nil || principal.Subject != "verified-approver" || principal.TenantID != "tenant-a" || !reflect.DeepEqual(principal.Roles, []string{"operator", "sre"}) {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	principal.Roles[0] = "changed"
	again, err := resolver.Resolve(req)
	if err != nil || !reflect.DeepEqual(again.Roles, []string{"operator", "sre"}) {
		t.Fatal("caller mutated future principal")
	}
	// Freshly issued membership removal is reflected on the next request.
	claims["groups"] = []string{"unknown-group"}
	req.Header.Set("Authorization", "Bearer "+principalToken(t, private, claims, "idp-key"))
	removed, err := resolver.Resolve(req)
	if err != nil || len(removed.Roles) != 0 {
		t.Fatal("removed membership retained")
	}
}

func TestOIDCPrincipalRejectsInvalidTrustAndRouting(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate-auth", "basic", "oversize", "issuer", "audience", "tenant", "tenant-type", "tenant-header", "duplicate-tenant", "missing-groups", "group-string", "group-type", "empty-group", "expired", "kid", "signature", "key-unavailable", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			config, private, claims := principalFixture(t)
			kid := "idp-key"
			switch kind {
			case "issuer":
				claims["iss"] = "https://other.example.test"
			case "audience":
				claims["aud"] = "themisy-tenant-b"
			case "tenant":
				claims["organization"] = "tenant-b"
			case "tenant-type":
				claims["organization"] = []string{"tenant-a"}
			case "missing-groups":
				delete(claims, "groups")
			case "group-string":
				claims["groups"] = "ops-team"
			case "group-type":
				claims["groups"] = []any{"ops-team", 42}
			case "empty-group":
				claims["groups"] = []string{""}
			case "expired":
				claims["exp"] = config.Verifier.Now().Unix()
			case "kid":
				kid = "unknown"
			case "key-unavailable":
				config.Verifier.Keys = identity.StaticOIDCKeys{}
			}
			resolver, err := NewOIDCIdentityResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			token := principalToken(t, private, claims, kid)
			if kind == "signature" {
				token = token[:strings.LastIndex(token, ".")+1] + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
			}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Actor-ID", "header-actor")
			req.Header.Set("X-Actor-Roles", "admin,sre")
			switch kind {
			case "missing":
				req.Header.Del("Authorization")
			case "duplicate-auth":
				req.Header.Add("Authorization", "Bearer "+token)
			case "basic":
				req.Header.Set("Authorization", "Basic "+token)
			case "oversize":
				req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 16384))
			case "tenant-header":
				req.Header.Set("X-Tenant-ID", "tenant-b")
			case "duplicate-tenant":
				req.Header.Add("X-Tenant-ID", "tenant-a")
				req.Header.Add("X-Tenant-ID", "tenant-a")
			case "cancel":
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			principal, err := resolver.Resolve(req)
			if !errors.Is(err, ErrUnauthenticatedPrincipal) || !reflect.DeepEqual(principal, Principal{}) {
				t.Fatalf("principal=%+v err=%v", principal, err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("token exposed in error")
			}
		})
	}
}

func TestOIDCPrincipalApprovalUsesSubjectAndRoles(t *testing.T) {
	for _, kind := range []string{"mapped", "header-escalation", "self-approval", "key-removed"} {
		t.Run(kind, func(t *testing.T) {
			service, memory, controller := testApplication(t)
			config, private, claims := principalFixture(t)
			if kind == "header-escalation" {
				claims["groups"] = []string{"unknown"}
			}
			if kind == "self-approval" {
				claims["sub"] = "requester"
			}
			resolver, err := NewOIDCIdentityResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			run := &domain.ReleaseRun{ID: "oidc-approval", WorkflowID: "oidc-approval", RequestID: "oidc-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "oidc-proof", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
			if _, _, err := memory.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			token := principalToken(t, private, claims, "idp-key")
			if kind == "key-removed" {
				probe := httptest.NewRequest(http.MethodPost, "/", nil)
				probe.Header.Set("Authorization", "Bearer "+token)
				if _, err := resolver.Resolve(probe); err != nil {
					t.Fatal(err)
				}
				delete(config.Verifier.Keys.(identity.StaticOIDCKeys), "https://idp.example.test\x00idp-key")
			}
			handler := NewWithIdentity(service, resolver, testLogger()).Handler()
			response := request(t, handler, http.MethodPost, "/v1/approvals/oidc-proof:approve", []byte(`{"actor":"forged","roles":["sre"]}`), map[string]string{"Authorization": "Bearer " + token, "X-Actor-ID": "forged", "X-Actor-Roles": "sre", "X-Tenant-ID": "tenant-a"})
			if kind == "mapped" {
				if response.Code != http.StatusOK || controller.lastApproval.Actor != "verified-approver" || !reflect.DeepEqual(controller.lastApproval.Roles, []string{"operator", "sre"}) {
					t.Fatalf("status=%d signal=%+v body=%s", response.Code, controller.lastApproval, response.Body.String())
				}
			} else if response.Code < 400 || controller.lastApproval.Actor != "" {
				t.Fatalf("rejection failed: status=%d signal=%+v", response.Code, controller.lastApproval)
			}
		})
	}
}

func TestOIDCPrincipalJWKSRefreshFailureStopsApproval(t *testing.T) {
	for _, kind := range []string{"unavailable", "removed"} {
		t.Run(kind, func(t *testing.T) {
			config, private, claims := principalFixture(t)
			var fail atomic.Bool
			var fetches atomic.Int32
			issuer := ""
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
				case "/keys":
					fetches.Add(1)
					if fail.Load() {
						if kind == "unavailable" {
							w.WriteHeader(http.StatusServiceUnavailable)
						} else {
							_, _ = w.Write([]byte(`{"keys":[]}`))
						}
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "idp-key", "use": "sig", "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey))}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			issuer = server.URL
			clock := config.Verifier.Now()
			jwks, err := identity.NewOIDCJWKSResolver(map[string]identity.OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/.well-known/openid-configuration", CacheTTL: time.Minute, HTTPClient: server.Client()}})
			if err != nil {
				t.Fatal(err)
			}
			jwks.Now = func() time.Time { return clock }
			config.Verifier.Issuer, config.Verifier.Keys, config.Verifier.Now = issuer, jwks, func() time.Time { return clock }
			claims["iss"] = issuer
			resolver, err := NewOIDCIdentityResolver(config)
			if err != nil {
				t.Fatal(err)
			}
			token := principalToken(t, private, claims, "idp-key")
			probe := httptest.NewRequest(http.MethodPost, "/", nil)
			probe.Header.Set("Authorization", "Bearer "+token)
			if _, err := resolver.Resolve(probe); err != nil {
				t.Fatal(err)
			}
			if fetches.Load() != 1 {
				t.Fatal("initial JWKS not fetched")
			}
			fail.Store(true)
			clock = clock.Add(61 * time.Second)
			service, memory, controller := testApplication(t)
			now := time.Now().UTC()
			run := &domain.ReleaseRun{ID: "jwks-approval", WorkflowID: "jwks-approval", RequestID: "jwks-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "jwks-proof", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
			if _, _, err := memory.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			response := request(t, NewWithIdentity(service, resolver, testLogger()).Handler(), http.MethodPost, "/v1/approvals/jwks-proof:approve", nil, map[string]string{"Authorization": "Bearer " + token, "X-Tenant-ID": "tenant-a", "X-Actor-ID": "forged", "X-Actor-Roles": "sre"})
			if response.Code < 400 || controller.lastApproval.Actor != "" || fetches.Load() != 2 {
				t.Fatalf("cached identity bypassed refresh: status=%d signal=%+v fetches=%d", response.Code, controller.lastApproval, fetches.Load())
			}
		})
	}
}
