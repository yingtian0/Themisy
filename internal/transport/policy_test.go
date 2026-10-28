package transport

import (
	"context"
	"net/http/httptest"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	policyfiles "themisy/policies"
	"time"
)

func TestPolicyDeliveryUsesAuthenticatedRunnerScope(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	signer, key, _ := grant.NewDevelopmentSigner("policy-key")
	baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
	bundle, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: "v1", Issuer: "policy", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}})
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	keys := policy.StaticBundleKeys{"policy\x00policy-key": key}
	d := &policy.Distributor{TenantID: "tenant", Issuer: "policy", Keys: keys, Store: memory, AllowedGroups: map[string]bool{"canary": true}}
	if _, err := d.Register(ctx, "tenant", "operator", "idp", bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Assign(ctx, "tenant", "canary", bundle.Hash, "operator", "idp", 0); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&RunnerServer{Store: memory, Audit: memory, Policies: d, Auth: StaticRunnerAuthenticator{"runner": {TenantID: "tenant", RunnerGroup: "canary", Token: "runner-token"}}}).Handler())
	defer server.Close()
	client := &RunnerClient{BaseURL: server.URL, RunnerID: "runner", TenantID: "tenant", RunnerGroup: "canary", Token: "runner-token", HTTP: server.Client()}
	source := &policy.RunnerPolicySource{TenantID: "tenant", RunnerGroup: "canary", Issuer: "policy", Keys: keys, Ledger: store.NewMemory(), Fetch: client.FetchPolicy, Acknowledge: client.AcknowledgePolicy}
	delivered, err := source.Load(ctx, "tenant", "canary")
	if err != nil || delivered.Bundle.Hash != bundle.Hash {
		t.Fatalf("delivery=%+v err=%v", delivered, err)
	}
	acknowledgements, err := memory.PolicyAcknowledgements(ctx, "tenant", "canary", 1)
	if err != nil || len(acknowledgements) != 1 || acknowledgements[0].RunnerID != "runner" {
		t.Fatalf("ack=%+v err=%v", acknowledgements, err)
	}
	if _, err := source.Load(ctx, "other-tenant", "canary"); err == nil {
		t.Fatal("cross-tenant policy accepted")
	}
	client.RunnerGroup = "other-group"
	if _, err := client.FetchPolicy(ctx); err == nil {
		t.Fatal("caller selected another group")
	}
	client.RunnerGroup = "canary"
	client.Token = "wrong"
	if _, err := client.FetchPolicy(ctx); err == nil {
		t.Fatal("unauthenticated policy read accepted")
	}
}
