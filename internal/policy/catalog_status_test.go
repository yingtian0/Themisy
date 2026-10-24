package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCandidateStatusRevalidatesWithoutDisclosingUntrustedMetadata(t *testing.T) {
	bundle, _, keys, now := shadowFixture(t)
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
	brokenHash := "sha256:" + strings.Repeat("f", 64)
	if err := os.WriteFile(filepath.Join(directory, brokenHash[7:]+".json"), []byte(`{"version":"raw-secret-canary"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".candidate-in-progress"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	inventory, err := catalog.Status(context.Background())
	if err != nil || len(inventory.Entries) != 2 || inventory.Mode != "LOCAL_CANDIDATE_STATUS_ONLY" {
		t.Fatal("inventory", err)
	}
	var good, bad CandidateStatus
	for _, row := range inventory.Entries {
		if row.Hash == bundle.Hash {
			good = row
		} else {
			bad = row
		}
	}
	if good.State != "VERIFIED_CANDIDATE" || good.Version != bundle.Version || bad.State != "UNAVAILABLE" || bad.Version != "" || bad.Issuer != "" {
		t.Fatal("untrusted metadata exposed or valid entry lost")
	}
	if inventory.Entries[0].Hash > inventory.Entries[1].Hash {
		t.Fatal("unstable ordering")
	}
	catalog.now = func() time.Time { return bundle.ExpiresAt }
	expired, err := catalog.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range expired.Entries {
		if row.State != "UNAVAILABLE" {
			t.Fatal("expired artifact still verified")
		}
	}
	catalog.now = func() time.Time { return now }
	delete(keys, bundle.Issuer+"\x00shadow-key")
	revoked, err := catalog.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range revoked.Entries {
		if row.State != "UNAVAILABLE" {
			t.Fatal("revoked trust still verified")
		}
	}
}
func TestCandidateStatusBoundsDirectoryAndRefusesCancelledInspection(t *testing.T) {
	bundle, _, keys, now := shadowFixture(t)
	directory := t.TempDir()
	catalog, err := OpenCandidateCatalog(directory, bundle.Issuer, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	catalog.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := catalog.Status(ctx); e == nil || len(r.Entries) != 0 {
		t.Fatal("cancelled status returned partial output")
	}
	for i := 0; i <= MaximumCandidateStatusEntries; i++ {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%064x.json", i)), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if r, e := catalog.Status(context.Background()); e == nil || len(r.Entries) != 0 {
		t.Fatal("oversized inventory returned partial positives")
	}
}
