package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/internal/store"
	policyfiles "themisy/policies"
	"time"
)

func TestPolicyAPIRequiresVerifiedAdminAndRejectsStaleUpdates(t *testing.T) {
	now := time.Now().UTC()
	signer, key, _ := grant.NewDevelopmentSigner("policy-key")
	baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
	ctx := context.Background()
	bundle, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: "v1", Issuer: "policy", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}})
	if err != nil {
		t.Fatal(err)
	}
	d := &policy.Distributor{TenantID: "tenant", Issuer: "policy", Keys: policy.StaticBundleKeys{"policy\x00policy-key": key}, Store: store.NewMemory(), AllowedGroups: map[string]bool{"canary": true}}
	handler := PolicyHandler(d)
	call := func(method, path string, input any, roles []string, verified bool) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("X-Actor-Roles", "policy-admin")
		if verified {
			r = r.WithContext(identity.WithRequestActor(r.Context(), identity.RequestActor{TenantID: "tenant", Subject: "operator", Issuer: "idp", SubjectType: "user", Roles: roles, ProofToken: "memory-only", ExpiresAt: now.Add(time.Hour)}, nil))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/v1/policy/bundles", bundle, nil, false); w.Code != 403 {
		t.Fatal("header supplied admin accepted")
	}
	if w := call("POST", "/v1/policy/bundles", bundle, []string{"sre"}, true); w.Code != 403 {
		t.Fatal("non-policy admin accepted")
	}
	if w := call("POST", "/v1/policy/bundles", bundle, []string{"policy-admin"}, true); w.Code != 201 {
		t.Fatalf("register=%d %s", w.Code, w.Body.String())
	}
	input := map[string]any{"bundle_hash": bundle.Hash, "expected_revision": 0}
	if w := call("PUT", "/v1/policy/assignments/canary", input, []string{"policy-admin"}, true); w.Code != 200 {
		t.Fatalf("assign=%d %s", w.Code, w.Body.String())
	}
	if w := call("PUT", "/v1/policy/assignments/canary", input, []string{"policy-admin"}, true); w.Code != 409 {
		t.Fatal("stale update accepted")
	}
	if w := call("GET", "/v1/policy/assignments/canary", nil, []string{"policy-admin"}, true); w.Code != 200 {
		t.Fatal("status unavailable")
	}
	if w := call("PUT", "/v1/policy/assignments/unknown", input, []string{"policy-admin"}, true); w.Code == 200 {
		t.Fatal("unconfigured rollout target accepted")
	}
}

var _ http.Handler = PolicyHandler(nil)
