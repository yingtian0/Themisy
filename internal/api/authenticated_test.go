package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

type authDirectorySigner struct{ key ed25519.PrivateKey }

func (s authDirectorySigner) Sign(_ context.Context, b []byte) (protocol.Signature, error) {
	return protocol.Signature{Algorithm: "Ed25519", KeyID: "dir", Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, b))}, nil
}
func TestProductionBoundaryRejectsHeadersAndRechecksDirectory(t *testing.T) {
	config, key, claims := principalFixture(t)
	now := config.Verifier.Now()
	sink := &identityAuditRecorder{}
	audited, err := NewAuditedOIDCIdentityResolver(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	directory := &identity.FileDirectory{Path: filepath.Join(t.TempDir(), "directory.json"), TenantID: config.TenantID, Issuer: "directory", MinimumRevision: 1, Keys: grant.StaticKeys{"directory\x00dir": key.Public().(ed25519.PublicKey)}, Ledger: &store.MemoryDirectoryLedger{}, Now: func() time.Time { return now }}
	bundle := identity.DirectoryBundle{Version: identity.DirectoryVersion, TenantID: config.TenantID, Issuer: directory.Issuer, Revision: 1, IssuedAt: now, ExpiresAt: now.Add(30 * time.Minute), Members: []identity.DirectoryMember{{Issuer: config.Verifier.Issuer, Subject: claims["sub"].(string), Enabled: true, Roles: []string{"sre"}}}}
	write := func() {
		t.Helper()
		b, err := identity.SignDirectory(context.Background(), authDirectorySigner{key}, "dir", bundle)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(b)
		if err := os.WriteFile(directory.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	resolver := &ProductionIdentityResolver{OIDC: audited, Directory: directory, Audit: sink, Now: func() time.Time { return now }}
	count := 0
	protected := Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := identity.RequestActorFromContext(r.Context())
		if !ok || a.Subject != claims["sub"] || !slices.Equal(a.Roles, []string{"sre"}) {
			t.Errorf("unexpected actor: %+v", a)
		}
		count++
		w.WriteHeader(204)
	}), resolver, func() time.Time { return now })
	token := principalToken(t, key, claims, "idp-key")
	for _, path := range []string{"/v1/releases", "/v1/plans", "/ui/approvals", "/mcp"} {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("X-Actor-ID", "admin")
		r.Header.Set("X-Actor-Roles", "admin,sre,deployer")
		r.Header.Set("X-Tenant-ID", config.TenantID)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("header-only access %s: %d", path, w.Code)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("verified access %s: %d", path, w.Code)
		}
	}
	if count != 4 {
		t.Fatal("unauthenticated handler executed")
	}
	bundle.Revision++
	bundle.Members[0].Enabled = false
	write()
	r := httptest.NewRequest("POST", "/ui/approvals", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, r)
	if w.Code != 401 || count != 4 {
		t.Fatal("revoked member entered handler")
	}
	bundle.Revision++
	bundle.Members[0].Enabled = true
	write()
	sink.append = func(context.Context, domain.AuditEvent) error { return context.DeadlineExceeded }
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, r)
	if w.Code != 401 || count != 4 {
		t.Fatal("audit outage entered handler")
	}
}
