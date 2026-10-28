package runner

import (
	"context"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	policyfiles "themisy/policies"
	"time"
)

func TestDistributedPolicyRejectsInvalidBundlesBeforeCredentials(t *testing.T) {
	for _, scenario := range []string{"valid", "tampered", "expired", "wrong revision", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRunnerFixture(t)
			ctx := context.Background()
			signer, key, _ := grant.NewDevelopmentSigner("policy-key")
			baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
			bundle, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: "policy-v1", Issuer: "policy", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Minute), Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}})
			if err != nil {
				t.Fatal(err)
			}
			delivery := policy.Delivery{Assignment: policy.Assignment{TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Revision: 1, BundleHash: bundle.Hash, UpdatedAt: f.now}, Bundle: bundle}
			keys := policy.StaticBundleKeys{"policy\x00policy-key": key}
			source := &policy.RunnerPolicySource{TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Issuer: "policy", Keys: keys, Ledger: store.NewMemory(), Now: func() time.Time { return f.now }, Fetch: func(context.Context) (policy.Delivery, error) {
				if scenario == "missing" {
					return policy.Delivery{}, policy.ErrDistribution
				}
				return delivery, nil
			}, Acknowledge: func(context.Context, policy.Assignment) error { return nil }}
			f.grant.PolicyHash = bundle.Hash
			f.grant.PolicyRevision = 1
			switch scenario {
			case "tampered":
				delivery.Bundle.Signature.Value = "invalid"
			case "expired":
				source.Now = func() time.Time { return f.now.Add(2 * time.Minute) }
			case "wrong revision":
				delivery.Assignment.Revision = 2
			}
			contexts := f.runner.Contexts.(StaticContexts)
			p := contexts[f.grant.RunID+"\x00"+f.grant.StepID]
			p.PolicyHash = bundle.Hash
			p.PolicyRevision = 1
			contexts[f.grant.RunID+"\x00"+f.grant.StepID] = p
			f.runner.Policy = policy.NewEngine(&policy.AssignedEvaluator{Load: source.Load, Issuer: "policy", Keys: keys, Now: func() time.Time { return f.now }})
			f.resignWithPolicyHash(t)
			_, err = f.runner.Execute(ctx, f.grant)
			if scenario == "valid" {
				if err != nil || f.adapter.Calls() != 1 {
					t.Fatalf("err=%v writes=%d", err, f.adapter.Calls())
				}
				return
			}
			if err == nil || f.credentials.Calls() != 0 || f.adapter.Calls() != 0 {
				t.Fatalf("invalid policy obtained credentials or wrote: err=%v credentials=%d writes=%d", err, f.credentials.Calls(), f.adapter.Calls())
			}
		})
	}
}
