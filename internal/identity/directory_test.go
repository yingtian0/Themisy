package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

type directorySigner struct{ key ed25519.PrivateKey }

func (s directorySigner) Sign(_ context.Context, p []byte) (protocol.Signature, error) {
	return protocol.Signature{Algorithm: "Ed25519", KeyID: "directory-key", Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, p))}, nil
}

func TestDirectoryRevocationRollbackAndBinding(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	f := &identity.FileDirectory{Path: filepath.Join(t.TempDir(), "directory.json"), Issuer: "https://directory.example", TenantID: "tenant-a", MinimumRevision: 1, Keys: grant.StaticKeys{"https://directory.example\x00directory-key": pub}, Ledger: &store.MemoryDirectoryLedger{}, Now: func() time.Time { return now }}
	base := identity.DirectoryBundle{Version: identity.DirectoryVersion, Issuer: f.Issuer, TenantID: f.TenantID, Revision: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(20 * time.Minute), Members: []identity.DirectoryMember{{Issuer: "https://idp.example", Subject: "alice", Enabled: true, Roles: []string{"deployer", "sre"}}}, Delegations: []identity.Delegation{{TenantID: f.TenantID, ID: "delegation", Issuer: "https://idp.example", UserSubject: "alice", AgentID: "agent", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"payment-*"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(10 * time.Minute)}}}
	write := func(b identity.DirectoryBundle, sign bool) {
		t.Helper()
		if sign {
			var err error
			b, err = identity.SignDirectory(ctx, directorySigner{key}, "directory-key", b)
			if err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(b)
		if err := os.WriteFile(f.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(base, true)
	if _, err := f.Member(ctx, "https://idp.example", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ResolveDelegation(ctx, "delegation"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*identity.DirectoryBundle){
		"tenant": func(b *identity.DirectoryBundle) { b.TenantID = "tenant-b" }, "issuer": func(b *identity.DirectoryBundle) { b.Issuer = "other" }, "expired": func(b *identity.DirectoryBundle) { b.ExpiresAt = now }, "future": func(b *identity.DirectoryBundle) { b.IssuedAt = now.Add(time.Second) }, "unbounded": func(b *identity.DirectoryBundle) { b.ExpiresAt = now.Add(2 * time.Hour) }, "duplicate member": func(b *identity.DirectoryBundle) { b.Members = append(b.Members, b.Members[0]) }, "delegation tenant": func(b *identity.DirectoryBundle) { b.Delegations[0].TenantID = "tenant-b" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var b identity.DirectoryBundle
			json.Unmarshal(raw, &b)
			mutate(&b)
			write(b, true)
			if _, err := f.Read(ctx); !errors.Is(err, identity.ErrDirectory) {
				t.Fatalf("accepted invalid directory: %v", err)
			}
		})
	}
	signed, _ := identity.SignDirectory(ctx, directorySigner{key}, "directory-key", base)
	signed.Revision = 2
	write(signed, false)
	if _, err := f.Read(ctx); err == nil {
		t.Fatal("accepted altered signature")
	}
	newer := base
	newer.Revision = 2
	newer.Members = nil
	newer.Delegations = nil
	write(newer, true)
	if _, err := f.Member(ctx, "https://idp.example", "alice"); err == nil {
		t.Fatal("revoked member accepted")
	}
	if _, err := f.ResolveDelegation(ctx, "delegation"); err == nil {
		t.Fatal("revoked delegation accepted")
	}
	write(base, true)
	if _, err := f.Read(ctx); err == nil {
		t.Fatal("rollback restored authority")
	}
	conflict := base
	conflict.Revision = 2
	write(conflict, true)
	if _, err := f.Read(ctx); err == nil {
		t.Fatal("same revision changed content accepted")
	}
	write(newer, true)
	if _, err := f.Read(ctx); err != nil {
		t.Fatal(err)
	}
	f.Keys = grant.StaticKeys{}
	if _, err := f.Read(ctx); err == nil {
		t.Fatal("removed trust key accepted")
	}
}
