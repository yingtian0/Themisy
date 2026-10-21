package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCandidateCatalogPersistsAndRevalidates(t *testing.T) {
	bundle, _, keys, now := shadowFixture(t)
	directory := t.TempDir()
	catalog, err := OpenCandidateCatalog(directory, bundle.Issuer, keys)
	if err != nil {
		t.Fatal(err)
	}
	catalog.now = func() time.Time { return now }
	hash, err := catalog.Register(context.Background(), bundle)
	if err != nil || hash != bundle.Hash {
		t.Fatalf("register: %s %v", hash, err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	// A fresh instance, not an in-memory map, must recover the exact object.
	catalog, err = OpenCandidateCatalog(directory, bundle.Issuer, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	catalog.now = func() time.Time { return now }
	got, err := catalog.Get(context.Background(), hash)
	if err != nil || got.Hash != hash || got.Signature != bundle.Signature {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Modules[0].Source = "changed caller copy"
	if _, err := catalog.Get(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	catalog.now = func() time.Time { return bundle.ExpiresAt }
	if _, err := catalog.Get(context.Background(), hash); err == nil {
		t.Fatal("expired cached object accepted")
	}
	if _, err := catalog.Register(context.Background(), bundle); err == nil {
		t.Fatal("expired registration accepted")
	}
	catalog.now = func() time.Time { return now }
	// Revocation changes configured trust, not the catalog file.
	delete(keys, "test-policy\x00shadow-key")
	if _, err := catalog.Get(context.Background(), hash); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func TestCandidateCatalogConcurrentImmutablePublication(t *testing.T) {
	bundle, _, keys, now := shadowFixture(t)
	directory := t.TempDir()
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			catalog, err := OpenCandidateCatalog(directory, bundle.Issuer, keys)
			if err != nil {
				errors <- err
				return
			}
			defer catalog.Close()
			catalog.now = func() time.Time { return now }
			_, err = catalog.Register(context.Background(), bundle)
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("leaked temporary files: %v %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
}

func TestCandidateCatalogRejectsBadArtifactsWithoutRepair(t *testing.T) {
	for _, kind := range []string{"tamper", "other-hash", "unknown-field", "trailing-json", "oversize", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			bundle, signer, keys, now := shadowFixture(t)
			directory := t.TempDir()
			catalog, err := OpenCandidateCatalog(directory, bundle.Issuer, keys)
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			catalog.now = func() time.Time { return now }
			if _, err := catalog.Register(context.Background(), bundle); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, strings.TrimPrefix(bundle.Hash, "sha256:")+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "tamper":
				data = []byte(strings.Replace(string(data), "current-v1", "forged-v1", 1))
			case "other-hash":
				other := signShadowCandidate(t, signer, bundle, "package themisy.authorization\nimport rego.v1\ndeny contains \"FROZEN\" if {true}")
				data, _ = json.Marshal(other)
			case "unknown-field":
				data = append([]byte(`{"unexpected":true,`), data[1:]...)
			case "trailing-json":
				data = append(data, []byte(` {}`)...)
			case "oversize":
				data = []byte(strings.Repeat(" ", maxCandidateBytes+1))
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "alias.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("alias.json", path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "symlink" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := catalog.Get(context.Background(), bundle.Hash); err == nil {
				t.Fatal("bad artifact accepted")
			}
			if _, err := catalog.Register(context.Background(), bundle); err == nil {
				t.Fatal("existing bad object silently repaired")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Fatal("registration replaced existing content")
			}
		})
	}
}

func TestCandidateCatalogRejectsBeforePublication(t *testing.T) {
	for _, kind := range []string{"signature", "issuer", "compile", "network", "cancel", "hash", "large"} {
		t.Run(kind, func(t *testing.T) {
			bundle, signer, keys, now := shadowFixture(t)
			directory := t.TempDir()
			catalog, err := OpenCandidateCatalog(directory, bundle.Issuer, keys)
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			catalog.now = func() time.Time { return now }
			ctx := context.Background()
			switch kind {
			case "signature":
				bundle.Signature.Value = "invalid"
			case "issuer":
				catalog.issuer = "wrong-issuer"
			case "compile":
				bundle = signShadowCandidate(t, signer, bundle, "not rego")
			case "network":
				bundle = signShadowCandidate(t, signer, bundle, `package themisy.authorization
import rego.v1
deny contains "REMOTE" if { http.send({"method":"GET", "url":"http://127.0.0.1:1"}).status_code == 200 }`)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "hash":
				bundle.Hash = "../../outside"
			case "large":
				bundle.Modules[0].Source = strings.Repeat("x", maxCandidateBytes)
			}
			if _, err := catalog.Register(ctx, bundle); err == nil {
				t.Fatal("invalid registration accepted")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid object persisted: %v %v", entries, err)
			}
			for _, hash := range []string{"../../outside", "sha256:../file", "sha256:" + strings.Repeat("A", 64), ""} {
				if _, err := catalog.Get(context.Background(), hash); err == nil {
					t.Fatal("invalid address accepted")
				}
			}
		})
	}
}

func TestCandidateCatalogExpiryDuringValidation(t *testing.T) {
	bundle, _, keys, now := shadowFixture(t)
	catalog, err := OpenCandidateCatalog(t.TempDir(), bundle.Issuer, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	calls := 0
	catalog.now = func() time.Time {
		calls++
		if calls > 1 {
			return bundle.ExpiresAt
		}
		return now
	}
	if _, err := catalog.Register(context.Background(), bundle); err == nil {
		t.Fatal("validation crossed expiry but succeeded")
	}
}
