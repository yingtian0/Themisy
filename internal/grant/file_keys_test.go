package grant

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileKeysSupportsRotationAndAppliesRevocationOnNextResolve(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	oldPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := testTrustBundle(now, "tenant-1",
		trustEntry("old", oldPublic, "active", now),
		trustEntry("new", newPublic, "active", now))
	resolver := writeTrustBundle(t, bundle, func() time.Time { return now })

	if got, err := resolver.ResolveGrantKey(context.Background(), bundle.Issuer, "old"); err != nil || string(got) != string(oldPublic) {
		t.Fatalf("old key during overlap: err=%v", err)
	}
	if got, err := resolver.ResolveGrantKey(context.Background(), bundle.Issuer, "new"); err != nil || string(got) != string(newPublic) {
		t.Fatalf("new key during overlap: err=%v", err)
	}

	bundle.Keys[0].Status = "revoked"
	rewriteTrustBundle(t, resolver.Path, bundle)
	if _, err := resolver.ResolveGrantKey(context.Background(), bundle.Issuer, "old"); err == nil {
		t.Fatal("revoked old key resolved after bundle refresh")
	}
	if got, err := resolver.ResolveGrantKey(context.Background(), bundle.Issuer, "new"); err != nil || string(got) != string(newPublic) {
		t.Fatalf("new key after old key revocation: err=%v", err)
	}
}

func TestFileKeysFailsClosedForInvalidTrustBundles(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	valid := testTrustBundle(now, "tenant-1", trustEntry("key-1", public, "active", now))
	tests := []struct {
		name   string
		mutate func(*GrantTrustBundle)
		raw    string
		want   string
	}{
		{"wrong issuer binding", func(b *GrantTrustBundle) { b.Issuer = "https://attacker.example" }, "", "binding"},
		{"wrong tenant binding", func(b *GrantTrustBundle) { b.TenantID = "tenant-2" }, "", "binding"},
		{"expired bundle", func(b *GrantTrustBundle) { b.ExpiresAt = now }, "", "valid"},
		{"future bundle", func(b *GrantTrustBundle) { b.IssuedAt = now.Add(time.Second) }, "", "valid"},
		{"unknown version", func(b *GrantTrustBundle) { b.Version = "v2" }, "", "version"},
		{"duplicate key ID", func(b *GrantTrustBundle) { b.Keys = append(b.Keys, b.Keys[0]) }, "", "duplicate"},
		{"same key under alias", func(b *GrantTrustBundle) { b.Keys = append(b.Keys, trustEntry("alias", public, "active", now)) }, "", "aliases"},
		{"unknown status", func(b *GrantTrustBundle) { b.Keys[0].Status = "enabled" }, "", "status"},
		{"invalid public key", func(b *GrantTrustBundle) { b.Keys[0].PublicKey = "bad" }, "", "Ed25519"},
		{"all revoked", func(b *GrantTrustBundle) { b.Keys[0].Status = "revoked" }, "", "revoked"},
		{"malformed JSON", nil, "{", "parse"},
		{"unknown field", nil, strings.Replace(mustTrustJSON(t, valid), "\"issuer\"", "\"unexpected\":\"x\",\"issuer\"", 1), "unknown JSON key"},
		{"duplicate JSON field", nil, strings.Replace(mustTrustJSON(t, valid), "\"issuer\":", "\"issuer\":\"https://attacker.example\",\"issuer\":", 1), "duplicate JSON key"},
		{"case folded field", nil, strings.Replace(mustTrustJSON(t, valid), "\"issuer\":", "\"Issuer\":", 1), "unknown JSON key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := valid
			bundle.Keys = append([]GrantTrustKey(nil), valid.Keys...)
			if test.mutate != nil {
				test.mutate(&bundle)
			}
			resolver := writeTrustBundle(t, bundle, func() time.Time { return now })
			resolver.Issuer, resolver.Tenant = valid.Issuer, valid.TenantID
			if test.raw != "" {
				if err := os.WriteFile(resolver.Path, []byte(test.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "key-1")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveGrantKey err=%v, want substring %q", err, test.want)
			}
		})
	}

	t.Run("unknown key ID", func(t *testing.T) {
		resolver := writeTrustBundle(t, valid, func() time.Time { return now })
		if _, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "missing"); err == nil {
			t.Fatal("unknown key ID resolved")
		}
	})
	t.Run("key outside validity", func(t *testing.T) {
		bundle := valid
		bundle.Keys = []GrantTrustKey{trustEntry("key-1", other, "active", now.Add(time.Minute))}
		resolver := writeTrustBundle(t, bundle, func() time.Time { return now })
		if _, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "key-1"); err == nil {
			t.Fatal("not-yet-valid key resolved")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		resolver := FileKeys{Path: filepath.Join(t.TempDir(), "missing.json"), Issuer: valid.Issuer, Tenant: "tenant-1", Now: func() time.Time { return now }}
		if _, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "key-1"); err == nil {
			t.Fatal("missing bundle accepted")
		}
	})
	t.Run("non-regular file", func(t *testing.T) {
		resolver := FileKeys{Path: t.TempDir(), Issuer: valid.Issuer, Tenant: valid.TenantID, Now: func() time.Time { return now }}
		if _, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "key-1"); err == nil {
			t.Fatal("directory accepted as a trust bundle")
		}
	})
	t.Run("oversized file", func(t *testing.T) {
		resolver := writeTrustBundle(t, valid, func() time.Time { return now })
		if err := os.WriteFile(resolver.Path, make([]byte, maxGrantTrustBundleBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveGrantKey(context.Background(), valid.Issuer, "key-1"); err == nil {
			t.Fatal("oversized bundle accepted")
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		resolver := writeTrustBundle(t, valid, func() time.Time { return now })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := resolver.ResolveGrantKey(ctx, valid.Issuer, "key-1"); err == nil {
			t.Fatal("cancelled resolution accepted")
		}
	})
}

func testTrustBundle(now time.Time, tenant string, keys ...GrantTrustKey) GrantTrustBundle {
	return GrantTrustBundle{Version: "themisy.grant-trust/v1", Issuer: "https://control.example", TenantID: tenant, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: keys}
}

func trustEntry(id string, key ed25519.PublicKey, status string, notBefore time.Time) GrantTrustKey {
	return GrantTrustKey{KeyID: id, PublicKey: base64.RawURLEncoding.EncodeToString(key), Status: status, NotBefore: notBefore, NotAfter: notBefore.Add(time.Hour)}
}

func writeTrustBundle(t *testing.T, bundle GrantTrustBundle, now func() time.Time) FileKeys {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grant-trust.json")
	rewriteTrustBundle(t, path, bundle)
	return FileKeys{Path: path, Issuer: bundle.Issuer, Tenant: bundle.TenantID, Now: now}
}

func rewriteTrustBundle(t *testing.T, path string, bundle GrantTrustBundle) {
	t.Helper()
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustTrustJSON(t *testing.T, bundle GrantTrustBundle) string {
	t.Helper()
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFileKeysPinnedBundleRefusesOldActiveKeyAfterRevocationAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	oldKey, _, _ := ed25519.GenerateKey(rand.Reader)
	newKey, _, _ := ed25519.GenerateKey(rand.Reader)
	old := testTrustBundle(now, "tenant-1", trustEntry("old", oldKey, "active", now), trustEntry("new", newKey, "active", now))
	resolver := writeTrustBundle(t, old, func() time.Time { return now })
	updated := old
	updated.Keys = append([]GrantTrustKey(nil), old.Keys...)
	updated.Keys[0].Status = "revoked"
	rewriteTrustBundle(t, resolver.Path, updated)
	raw, err := os.ReadFile(resolver.Path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	resolver.ExpectedHash = "sha256:" + hex.EncodeToString(hash[:])
	if _, err := resolver.ResolveGrantKey(context.Background(), old.Issuer, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveGrantKey(context.Background(), old.Issuer, "old"); err == nil {
		t.Fatal("revoked key accepted")
	}
	rewriteTrustBundle(t, resolver.Path, old)
	restarted := FileKeys{Path: resolver.Path, Issuer: resolver.Issuer, Tenant: resolver.Tenant, ExpectedHash: resolver.ExpectedHash, Now: resolver.Now}
	if restarted.Validate(context.Background()) == nil {
		t.Fatal("old bundle passed startup validation")
	}
	if _, err := restarted.ResolveGrantKey(context.Background(), old.Issuer, "old"); err == nil {
		t.Fatal("old active key restored")
	}
	rewriteTrustBundle(t, resolver.Path, updated)
	if _, err := restarted.ResolveGrantKey(context.Background(), old.Issuer, "new"); err != nil {
		t.Fatal(err)
	}
}
