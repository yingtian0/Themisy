package application

import (
	"context"
	"fmt"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	policyfiles "themisy/policies"
	"time"
)

type policyDurableStore interface {
	store.DurableStore
	policy.DistributionStore
}

func TestGrantRejectsPolicyRolloutAndRollbackUnderOldApproval(t *testing.T) {
	testPolicyGrantAdmission(t, store.NewMemory())
}
func testPolicyGrantAdmission(t *testing.T, st policyDurableStore) {
	t.Helper()
	for _, scenario := range []string{"valid", "changed during signing", "returned to old bundle", "changed at durable dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			run := grantRun(now)
			run.ID = fmt.Sprintf("policy-grant-%d", now.UnixNano())
			run.RequestID = run.ID
			run.WorkflowID = run.ID
			run.TenantID = run.ID
			signer, key, _ := grant.NewDevelopmentSigner("policy-key")
			baseline, _ := policyfiles.FS.ReadFile("baseline/main.rego")
			keys := policy.StaticBundleKeys{"policy\x00policy-key": key}
			d := &policy.Distributor{TenantID: run.TenantID, Issuer: "policy", Keys: keys, Store: st, AllowedGroups: map[string]bool{"payments-prod": true}}
			bundles := []policy.Bundle{}
			for _, version := range []string{"v1", "v2"} {
				b, err := policy.SignBundle(ctx, signer, policy.Bundle{Version: version, Issuer: d.Issuer, Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(baseline)}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.Register(ctx, run.TenantID, "admin", "idp", b); err != nil {
					t.Fatal(err)
				}
				bundles = append(bundles, b)
			}
			if _, err := d.Assign(ctx, run.TenantID, "payments-prod", bundles[0].Hash, "admin", "idp", 0); err != nil {
				t.Fatal(err)
			}
			run.Plan.PolicyHash = bundles[0].Hash
			run.Plan.PolicyAssignments = map[string]int64{"payments-prod": 1}
			if _, _, err := st.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			grantSigner, _, _ := grant.NewDevelopmentSigner("grant-key")
			service, err := NewGrants(st, grantSigner, "control", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			service.Policy = policy.NewEngine(&policy.AssignedEvaluator{Load: d.Current, Issuer: d.Issuer, Keys: keys})
			update := func() {
				t.Helper()
				if _, err := d.Assign(ctx, run.TenantID, "payments-prod", bundles[1].Hash, "admin", "idp", 1); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed at durable dispatch" {
				service.Store = policyDispatchRaceStore{policyDurableStore: st, before: update}
			}
			if scenario == "changed during signing" {
				service.Signer = hookSigner{delegate: grantSigner, hook: update}
			}
			if scenario == "returned to old bundle" {
				update()
				if _, err := d.Rollback(ctx, run.TenantID, "payments-prod", "admin", "idp", 2); err != nil {
					t.Fatal(err)
				}
			}
			record, created, err := service.Issue(ctx, GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if scenario == "valid" {
				if err != nil || !created || record.Grant.PolicyRevision != 1 || record.Grant.PolicyHash != bundles[0].Hash {
					t.Fatalf("record=%+v created=%v err=%v", record, created, err)
				}
				return
			}
			if err == nil || created {
				t.Fatalf("old approval authorized policy change: created=%v err=%v", created, err)
			}
		})
	}
}

type policyDispatchRaceStore struct {
	policyDurableStore
	before func()
}

func (s policyDispatchRaceStore) CreateGrantDispatch(ctx context.Context, record store.GrantDispatchRecord, audit domain.AuditEvent, outbox domain.OutboxEvent) (store.GrantDispatchRecord, bool, error) {
	s.before()
	return s.policyDurableStore.CreateGrantDispatch(ctx, record, audit, outbox)
}
