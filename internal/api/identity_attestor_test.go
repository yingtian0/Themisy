package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func TestIndependentIdentityExchangeVerifiesAndAuditsWithoutBearerPersistence(t *testing.T) {
	cfg, key, claims := principalFixture(t)
	cfg.RoleMappings = map[string][]string{"ops-team": {"deployer"}}
	now := cfg.Verifier.Now()
	ctx := context.Background()
	signer, public, _ := grant.NewDevelopmentSigner("identity-key")
	grantSigner, _, _ := grant.NewDevelopmentSigner("grant-key")
	sink := &identityAuditRecorder{}
	directory := &identity.FileDirectory{Path: filepath.Join(t.TempDir(), "directory.json"), Issuer: "directory", TenantID: cfg.TenantID, MinimumRevision: 1, Keys: grant.StaticKeys{"directory\x00dir": key.Public().(ed25519.PublicKey)}, Ledger: &store.MemoryDirectoryLedger{}, Now: func() time.Time { return now }}
	bundle := identity.DirectoryBundle{Version: identity.DirectoryVersion, Issuer: directory.Issuer, TenantID: cfg.TenantID, Revision: 1, IssuedAt: now, ExpiresAt: now.Add(30 * time.Minute), Members: []identity.DirectoryMember{{Issuer: cfg.Verifier.Issuer, Subject: claims["sub"].(string), Enabled: true, Roles: []string{"deployer"}}}, Delegations: []identity.Delegation{{TenantID: cfg.TenantID, ID: "delegation", Issuer: cfg.Verifier.Issuer, UserSubject: claims["sub"].(string), AgentID: "agent", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(20 * time.Minute)}}}
	write := func() {
		t.Helper()
		signed, err := identity.SignDirectory(ctx, authDirectorySigner{key}, "dir", bundle)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(signed)
		if err := os.WriteFile(directory.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	audited, err := NewAuditedOIDCIdentityResolver(cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &ProductionIdentityResolver{OIDC: audited, Directory: directory, Audit: sink, Now: func() time.Time { return now }}
	verifier := &identity.AttestationVerifier{Issuer: "independent-identity", TenantID: cfg.TenantID, Audience: cfg.Verifier.Audience, Keys: grant.StaticKeys{"independent-identity\x00identity-key": public}, OIDC: &cfg.Verifier, Directory: directory, Now: func() time.Time { return now }}
	handler := Authenticate(&IdentityAttestor{Verifier: verifier, Signer: signer, KeyID: "identity-key", Audit: sink}, resolver, func() time.Time { return now })
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	remote := &identity.RemoteAttester{URL: server.URL, HTTP: server.Client()}
	token := principalToken(t, key, claims, "idp-key")
	input := identity.AttestationRequest{RequestID: "request", AgentID: "agent", DelegationRef: "delegation"}
	proof, err := remote.Exchange(ctx, token, input)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := verifier.VerifyAttestation(ctx, proof)
	if err != nil || receipt.SubjectID != claims["sub"] {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	raw, _ := json.Marshal(sink.events)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), proof) {
		t.Fatal("credential or proof persisted in identity audit")
	}
	forged, err := identity.Attest(ctx, &cfg.Verifier, grantSigner, "grant-key", identity.Attestation{Issuer: verifier.Issuer, TenantID: cfg.TenantID, Audience: cfg.Verifier.Audience, SubjectType: "user", RequestID: input.RequestID, AgentID: input.AgentID, DelegationRef: input.DelegationRef, Roles: []string{"deployer"}}, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyAttestation(ctx, forged); err == nil {
		t.Fatal("Grant signing authority forged an identity receipt")
	}
	input.AgentID = "other-agent"
	if _, err := remote.Exchange(ctx, token, input); err == nil {
		t.Fatal("accepted other agent")
	}
	input.AgentID = "agent"
	bundle.Revision++
	bundle.Members[0].Enabled = false
	write()
	if _, err := remote.Exchange(ctx, token, input); err == nil {
		t.Fatal("revoked member obtained new proof")
	}
}
func TestIdentityExchangeNeverFollowsRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"proof":"unexpected"}`)) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	remote := &identity.RemoteAttester{URL: redirect.URL, HTTP: redirect.Client()}
	if _, err := remote.Exchange(context.Background(), "token", identity.AttestationRequest{RequestID: "request", AgentID: "agent", DelegationRef: "delegation"}); err == nil || calls != 0 {
		t.Fatal("redirect forwarded bearer authorization")
	}
}
