package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/store"
	"time"
)

func TestAttestationContainsNoTokenAndRechecksCurrentTrust(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	idpPublic, idpKey, _ := ed25519.GenerateKey(rand.Reader)
	cpPublic, cpKey, _ := ed25519.GenerateKey(rand.Reader)
	issuer := "https://idp.example"
	oidcKeys := identity.StaticOIDCKeys{issuer + "\x00idp-key": idpPublic}
	oidc := &identity.OIDCVerifier{Issuer: issuer, Audience: "themisy", Keys: oidcKeys, Now: func() time.Time { return now }}
	h, _ := json.Marshal(map[string]any{"alg": "EdDSA", "kid": "idp-key"})
	b, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "alice", "aud": "themisy", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "private": "secret-claim-canary"})
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	token := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(idpKey, []byte(input)))
	dir := &identity.FileDirectory{Path: filepath.Join(t.TempDir(), "directory.json"), Issuer: "directory", TenantID: "tenant-a", MinimumRevision: 1, Keys: grant.StaticKeys{"directory\x00directory-key": cpPublic}, Ledger: &store.MemoryDirectoryLedger{}, Now: func() time.Time { return now }}
	bundle := identity.DirectoryBundle{Version: identity.DirectoryVersion, Issuer: "directory", TenantID: "tenant-a", Revision: 1, IssuedAt: now, ExpiresAt: now.Add(20 * time.Minute), Members: []identity.DirectoryMember{{Issuer: issuer, Subject: "alice", Enabled: true, Roles: []string{"deployer"}}}}
	write := func() {
		t.Helper()
		signed, err := identity.SignDirectory(ctx, directorySigner{cpKey}, "directory-key", bundle)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(signed)
		if err := os.WriteFile(dir.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	proof, err := identity.Attest(ctx, oidc, directorySigner{cpKey}, "directory-key", identity.Attestation{Issuer: "control", TenantID: "tenant-a", Audience: "themisy", SubjectType: "ci", RequestID: "request", AgentID: "agent", DelegationRef: "delegation", Roles: []string{"deployer"}}, token)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(proof, "themisy-identity-v1:"))
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "secret-claim-canary") {
		t.Fatal("bearer credential or private claim retained")
	}
	verifier := &identity.AttestationVerifier{Issuer: "control", TenantID: "tenant-a", Audience: "themisy", Keys: grant.StaticKeys{"control\x00directory-key": cpPublic}, OIDC: oidc, Directory: dir, Now: func() time.Time { return now }}
	subject, err := verifier.Verify(ctx, proof)
	if err != nil || subject.ID != "alice" || subject.Claims["subject_type"] != "ci" {
		t.Fatalf("subject=%+v err=%v", subject, err)
	}
	for _, mutation := range []func(){func() { verifier.TenantID = "tenant-b" }, func() { verifier.Audience = "other" }, func() { verifier.Issuer = "other" }, func() { oidcKeys[issuer+"\x00idp-key"] = cpPublic }, func() { verifier.Keys = grant.StaticKeys{} }} {
		old := *verifier
		mutation()
		if _, err := verifier.Verify(ctx, proof); err == nil {
			t.Fatal("invalid identity trust accepted")
		}
		*verifier = old
		oidcKeys[issuer+"\x00idp-key"] = idpPublic
	}
	tampered := string(raw)
	tampered = strings.Replace(tampered, "alice", "mallory", 1)
	if _, err := verifier.Verify(ctx, "themisy-identity-v1:"+base64.RawURLEncoding.EncodeToString([]byte(tampered))); err == nil {
		t.Fatal("tampered subject accepted")
	}
	bundle.Revision = 2
	bundle.Members[0].Enabled = false
	write()
	if _, err := verifier.Verify(ctx, proof); err == nil {
		t.Fatal("revoked member accepted")
	}
	bundle.Revision = 3
	bundle.Members[0].Enabled = true
	write()
	now = now.Add(5 * time.Minute)
	if _, err := verifier.Verify(ctx, proof); err == nil {
		t.Fatal("expired attestation accepted")
	}
}
