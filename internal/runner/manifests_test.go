package runner

import (
	"context"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func fixtureManifest(t *testing.T, f *runnerFixture) protocol.PlanManifest {
	t.Helper()
	g := f.grant
	p := protocol.PlanManifest{ProtocolVersion: protocol.PlanManifestVersion, Issuer: "https://approvals.example", TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, RunID: g.RunID, StepID: g.StepID, Revision: 1, PlanHash: g.PlanHash, ContractHash: g.ContractHash, ProfileHash: g.ProfileHash, PolicyHash: g.PolicyHash, EvidenceHash: g.EvidenceHash, Target: g.Target, Action: g.Action, ApprovalRequired: true, ApprovalProofs: []string{"approval-1"}, IssuedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Minute)}
	p, err := grant.SignPlanManifest(context.Background(), f.signer, p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIndependentManifestRejectsResignedGrantChanges(t *testing.T) {
	changes := map[string]func(*protocol.ActionGrant){
		"plan":     func(g *protocol.ActionGrant) { g.PlanHash = testDigest("b") },
		"contract": func(g *protocol.ActionGrant) { g.ContractHash = testDigest("b") },
		"profile":  func(g *protocol.ActionGrant) { g.ProfileHash = testDigest("b") },
		"evidence": func(g *protocol.ActionGrant) { g.EvidenceHash = testDigest("b") },
		"policy":   func(g *protocol.ActionGrant) { g.PolicyHash = testDigest("b") },
		"target":   func(g *protocol.ActionGrant) { g.Target.Service = "other" },
		"action":   func(g *protocol.ActionGrant) { g.Action.ArtifactDigest = testDigest("b") },
		"approval": func(g *protocol.ActionGrant) { g.ApprovalProofs = []string{"other"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newRunnerFixture(t)
			// Keep identity/delegation valid for the alternate target so this
			// case exercises independent manifest matching, not scope rejection.
			f.delegation.ServiceSelectors = append(f.delegation.ServiceSelectors, "other")
			f.refreshDelegation()
			m := &ManifestContexts{Store: f.journal, Issuer: "https://approvals.example", TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Keys: grant.StaticKeys{"https://approvals.example\x00grant-key": f.public}, Now: func() time.Time { return f.now }}
			if err := m.Pin(context.Background(), fixtureManifest(t, f)); err != nil {
				t.Fatal(err)
			}
			f.runner.Contexts = m
			f.runner.Approvals = m
			change(&f.grant)
			f.resignWithPolicyHash(t)
			if _, err := f.runner.Execute(context.Background(), f.grant); err == nil {
				t.Fatal("modified grant accepted")
			}
			if f.credentials.Calls() != 0 || f.adapter.Calls() != 0 {
				t.Fatal("rejection occurred after side effects")
			}
		})
	}
}

func TestManifestLifecycleFailsClosed(t *testing.T) {
	f := newRunnerFixture(t)
	ctx := context.Background()
	m := &ManifestContexts{Store: store.NewMemory(), Issuer: "https://approvals.example", TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Keys: grant.StaticKeys{"https://approvals.example\x00grant-key": f.public}, Now: func() time.Time { return f.now }}
	f.runner.Contexts = m
	f.runner.Approvals = m
	if _, err := f.runner.Execute(ctx, f.grant); err == nil {
		t.Fatal("missing manifest accepted")
	}
	p := fixtureManifest(t, f)
	bad := p
	bad.PlanHash = testDigest("c")
	if err := m.Pin(ctx, bad); err == nil {
		t.Fatal("invalid signature accepted")
	}
	if err := m.Pin(ctx, p); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return p.ExpiresAt }
	if _, err := f.runner.Execute(ctx, f.grant); err == nil {
		t.Fatal("expired manifest accepted")
	}
	m.Now = func() time.Time { return f.now }
	next := p
	next.Revision = 2
	next.PlanHash = testDigest("b")
	next.ApprovalProofs = []string{"approval-2"}
	next, err := grant.SignPlanManifest(ctx, f.signer, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Pin(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := m.Pin(ctx, p); err == nil {
		t.Fatal("revision rollback accepted")
	}
	if _, err := f.runner.Execute(ctx, f.grant); err == nil {
		t.Fatal("old grant accepted after plan change")
	}
	if f.credentials.Calls() != 0 || f.adapter.Calls() != 0 {
		t.Fatal("unsafe lifecycle caused side effect")
	}
}

func TestManifestAllowsMatchingApprovedPlan(t *testing.T) {
	f := newRunnerFixture(t)
	m := &ManifestContexts{Store: f.journal, Issuer: "https://approvals.example", TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Keys: grant.StaticKeys{"https://approvals.example\x00grant-key": f.public}, Now: func() time.Time { return f.now }}
	if err := m.Pin(context.Background(), fixtureManifest(t, f)); err != nil {
		t.Fatal(err)
	}
	f.runner.Contexts = m
	f.runner.Approvals = m
	if _, err := f.runner.Execute(context.Background(), f.grant); err != nil {
		t.Fatal(err)
	}
	if f.adapter.Calls() != 1 {
		t.Fatal("expected one write")
	}
}
