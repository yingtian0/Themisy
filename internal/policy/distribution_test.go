package policy_test

import (
	"context"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	policyfiles "themisy/policies"
	"time"
)

func TestScopedCanaryRollbackAndRevisionFence(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	memory := store.NewMemory()
	signer, key, _ := grant.NewDevelopmentSigner("policy-key")
	baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
	makeBundle := func(version string, extra bool) policy.Bundle {
		t.Helper()
		modules := []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}
		if extra {
			modules = append(modules, policy.Module{Name: "freeze.rego", Layer: policy.LayerService, Source: `package themisy.authorization
import rego.v1
deny contains "SERVICE_FREEZE" if { input.service == "payment-api" }`})
		}
		b, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: version, Issuer: "policy", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: modules})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	before, after := makeBundle("v1", false), makeBundle("v2", true)
	d := &policy.Distributor{TenantID: "tenant", Issuer: "policy", Keys: policy.StaticBundleKeys{"policy\x00policy-key": key}, Store: memory, AllowedGroups: map[string]bool{"canary": true, "stable": true}, Now: func() time.Time { return now }}
	for _, b := range []policy.Bundle{before, after} {
		if _, err := d.Register(ctx, "tenant", "operator", "idp", b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Assign(ctx, "other-tenant", "canary", before.Hash, "operator", "idp", 0); err == nil {
		t.Fatal("cross-tenant assignment accepted")
	}
	for _, group := range []string{"canary", "stable"} {
		if _, err := d.Assign(ctx, "tenant", group, before.Hash, "operator", "idp", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Assign(ctx, "tenant", "canary", after.Hash, "operator", "idp", 1); err != nil {
		t.Fatal(err)
	}
	stable, err := d.Current(ctx, "tenant", "stable")
	if err != nil || stable.Bundle.Hash != before.Hash {
		t.Fatal("canary changed stable group")
	}
	current, err := d.Current(ctx, "tenant", "canary")
	if err != nil || current.Assignment.Revision != 2 {
		t.Fatal(err)
	}
	if _, err := d.Assign(ctx, "tenant", "canary", before.Hash, "operator", "idp", 1); err == nil {
		t.Fatal("stale assignment CAS accepted")
	}
	engine := policy.NewEngine(&policy.AssignedEvaluator{Load: d.Current, Issuer: d.Issuer, Keys: d.Keys, Now: func() time.Time { return now }})
	input := policy.Input{TenantID: "tenant", RunnerGroup: "canary", PolicyHash: after.Hash, PolicyRevision: 2, UserID: "user", AgentID: "agent", Environment: domain.EnvironmentStaging, Service: "payment-api", Risk: "low"}
	result, err := engine.EvaluateContext(ctx, input)
	if err != nil || result.Decision != domain.DecisionDeny {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if d.Acknowledge(ctx, "tenant", "canary", "runner", 2, after.Hash) != nil {
		t.Fatal("delivery acknowledgement failed")
	}
	acks, _ := memory.PolicyAcknowledgements(ctx, "tenant", "canary", 2)
	if len(acks) != 1 {
		t.Fatal("delivery status missing")
	}
	if memory.AcceptPolicyRevision(ctx, current.Assignment) != nil {
		t.Fatal("revision fence failed")
	}
	rollback, err := d.Rollback(ctx, "tenant", "canary", "operator", "idp", 2)
	if err != nil || rollback.Revision != 3 || rollback.BundleHash != before.Hash {
		t.Fatalf("rollback=%+v err=%v", rollback, err)
	}
	if memory.AcceptPolicyRevision(ctx, rollback) != nil || memory.AcceptPolicyRevision(ctx, current.Assignment) == nil {
		t.Fatal("old delivery replay accepted")
	}
	// Returning to the same signed bundle does not revive an old Plan/approval.
	input.PolicyHash = before.Hash
	input.PolicyRevision = 1
	if _, err := engine.EvaluateContext(ctx, input); err == nil {
		t.Fatal("old generation accepted after rollback")
	}
	input.PolicyRevision = 3
	if result, err := engine.EvaluateContext(ctx, input); err != nil || result.Decision != domain.DecisionAllow {
		t.Fatalf("rollback result=%+v err=%v", result, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := engine.EvaluateContext(ctx, input); err == nil {
		t.Fatal("expired policy accepted")
	}
}
