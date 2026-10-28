//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/policy"
	policyfiles "themisy/policies"
	"time"
)

func TestPolicyDistributionSurvivesRestart(t *testing.T) {
	url := os.Getenv("THEMISY_IDENTITY_DIRECTORY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires owned Policy fixture")
	}
	ctx := context.Background()
	if err := Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	if err := MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	journal, err := NewRunnerJournal(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	now := time.Now().UTC()
	signer, key, _ := grant.NewDevelopmentSigner("policy-key")
	baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
	d := &policy.Distributor{TenantID: "policy-fixture", Issuer: "policy", Keys: policy.StaticBundleKeys{"policy\x00policy-key": key}, Store: st, AllowedGroups: map[string]bool{"canary": true, "stable": true}}
	hashes := []string{}
	for _, version := range []string{"v1", "v2"} {
		bundle, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: version, Issuer: "policy", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}})
		if err != nil {
			t.Fatal(err)
		}
		hash, err := d.Register(ctx, d.TenantID, "operator", "idp", bundle)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
	}
	first, err := d.Assign(ctx, d.TenantID, "canary", hashes[0], "operator", "idp", 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := d.Assign(ctx, d.TenantID, "canary", hashes[1], "operator", "idp", first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.AcceptPolicyRevision(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := d.Acknowledge(ctx, d.TenantID, "canary", "runner", next.Revision, next.BundleHash); err != nil {
		t.Fatal(err)
	}
	st.Close()
	journal.Close()
	st, err = New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	journal, err = NewRunnerJournal(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	d.Store = st
	if journal.AcceptPolicyRevision(ctx, first) == nil {
		t.Fatal("reopening journal permitted old delivery")
	}
	if _, err := d.Assign(ctx, d.TenantID, "canary", hashes[0], "operator", "idp", first.Revision); err == nil {
		t.Fatal("reopening store permitted old CAS")
	}
	acks, err := st.PolicyAcknowledgements(ctx, d.TenantID, "canary", next.Revision)
	if err != nil || len(acks) != 1 {
		t.Fatalf("acks=%+v err=%v", acks, err)
	}
	rollback, err := d.Rollback(ctx, d.TenantID, "canary", "operator", "idp", next.Revision)
	if err != nil || rollback.Revision != 3 || rollback.BundleHash != hashes[0] {
		t.Fatalf("rollback=%+v err=%v", rollback, err)
	}
	if journal.AcceptPolicyRevision(ctx, rollback) != nil {
		t.Fatal("legitimate new rollback revision rejected")
	}
	events, err := st.AuditEvents("policy/" + d.TenantID)
	if err != nil || len(events) != 6 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}
