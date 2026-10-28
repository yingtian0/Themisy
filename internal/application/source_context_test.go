package application

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"themisy/internal/contextsnapshot"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/planner"
	"themisy/internal/policy"
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
	"themisy/test/sourcefixture"
	"time"
)

type sourceOperations struct {
	*observationFlowSource
	credentials int
	tenant      string
	before      func()
}

func (s *sourceOperations) Acquire(ctx context.Context, r credentials.Request) (credentials.Credential, error) {
	s.credentials++
	if s.before != nil {
		s.before()
	}
	if r.TenantID != s.tenant || r.Service != "payment-api" {
		return credentials.Credential{}, fmt.Errorf("credential scope mismatch")
	}
	return credentials.New(map[string][]byte{"token": []byte("deployment-fixture")}, time.Now().Add(time.Hour)), nil
}

type sourceDurableStore interface {
	store.DurableStore
	contextsnapshot.ContextLedger
	contextsnapshot.CIAuditSink
}

func TestSignedSourcesControlPlanGrantAndRunner(t *testing.T) { testSignedSources(t, nil) }
func testSignedSources(t *testing.T, shared sourceDurableStore) {
	for _, scenario := range []string{"valid", "caller success without source", "wrong artifact", "wrong commit", "changed before Grant", "provider failure before Runner", "source expired", "untrusted metadata revision", "provider failure during credentials", "changed at dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			var memory sourceDurableStore = store.NewMemory()
			if shared != nil {
				memory = shared
			}
			fixture := sourcefixture.New(t, memory, fmt.Sprintf("source-fixture-%d", time.Now().UnixNano()))
			cpSource := fixture.Source(t, memory, memory)
			authority := &SourceAuthority{Source: cpSource}
			verified, err := cpSource.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := policy.NewMandatoryEngine(ctx)
			if err != nil {
				t.Fatal(err)
			}
			base, err := planner.NewFromContracts(verified.Contracts(), verified.Profiles(), planner.Options{PolicyHash: engine.PolicyHash(), EvidenceHash: policy.PreDispatchEvidenceHash()})
			if err != nil {
				t.Fatal(err)
			}
			releases := NewReleases(base, memory, &fakeWorkflowController{})
			releases.Sources = authority
			intent := domain.ReleaseIntent{APIVersion: domain.ReleaseIntentAPIVersion, Kind: domain.ReleaseIntentKind, RequestID: fixture.TenantID + "-release", ReleaseVersion: "v1", TenantID: fixture.TenantID, Environment: domain.EnvironmentProduction, RequestedBy: "user-1", SubjectType: "user", SubjectIssuer: "https://idp.example", UserIdentityProof: "verified-token", DelegationRef: "delegation-1", Agent: domain.AgentIdentity{ID: "agent-1"}, Changes: []domain.Change{{Service: "payment-api", DesiredVersion: fixture.Artifact, CISuccess: false, DependenciesHealthy: false}}}
			if scenario == "caller success without source" {
				fixture.Provider.Failure.Store(true)
				fixture.Publish(t, 2)
				intent.Changes[0].CISuccess = true
				intent.Changes[0].DependenciesHealthy = true
			}
			if scenario == "wrong artifact" {
				intent.Changes[0].DesiredVersion = "sha256:" + strings.Repeat("e", 64)
				intent.Changes[0].CISuccess = true
				intent.Changes[0].DependenciesHealthy = true
			}
			if scenario == "wrong commit" {
				intent.Changes[0].CommitSHA = strings.Repeat("e", 40)
			}
			run, created, err := releases.StartIntentForTenant(ctx, fixture.TenantID, intent)
			if scenario == "caller success without source" || scenario == "wrong artifact" || scenario == "wrong commit" {
				if err == nil || created {
					t.Fatal("self assertion or wrong source created a release")
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("start created=%v err=%v", created, err)
			}
			if run.Plan.Context == nil || run.Plan.EvidenceHash != verified.Hash() || !run.Steps[0].Change.CISuccess || !run.Steps[0].Change.DependenciesHealthy || run.Steps[0].Change.CommitSHA != fixture.Commit {
				t.Fatalf("trusted facts not derived: %+v", run.Plan.Context)
			}
			run.Status = domain.RunRunning
			run.Steps[0].Status = domain.StepExecuting
			run.Steps[0].Policy = &domain.PolicyDecision{Decision: domain.DecisionRequireApproval, InputHash: "sha256:" + strings.Repeat("a", 64)}
			decided := time.Now().UTC()
			run.Steps[0].Approval = &domain.Approval{ID: "approval-1", Status: domain.ApprovalApproved, PlanHash: run.Plan.Hash, DecidedAt: &decided, DecidedBy: "owner", RequiredRoles: []string{"service-owner"}, ExpiresAt: run.Plan.ExpiresAt}
			if err := memory.UpdateRun(run, run.StateVersion); err != nil {
				t.Fatal(err)
			}
			signer, key, _ := grant.NewDevelopmentSigner("grant-key")
			issuer, err := NewGrants(memory, signer, "https://control.example", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			issuer.Sources = authority
			issuer.Policy = engine
			if scenario == "changed at dispatch" {
				issuer.Store = sourceDispatchRaceStore{sourceDurableStore: memory, before: func() {
					fixture.Publish(t, 2)
					if _, err := cpSource.Read(ctx); err != nil {
						t.Fatal(err)
					}
				}}
			}
			if scenario == "changed before Grant" {
				fixture.Publish(t, 2)
			}
			request := GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy}
			record, fresh, err := issuer.Issue(ctx, request)
			if scenario == "changed before Grant" || scenario == "changed at dispatch" {
				if err == nil || fresh {
					t.Fatal("changed Context reused approval")
				}
				return
			}
			if err != nil || !fresh {
				t.Fatalf("issue fresh=%v err=%v", fresh, err)
			}
			if record.Grant.ContextRevision != 1 || record.Grant.EvidenceHash != run.Plan.Context.Hash || record.Grant.ExpiresAt.After(run.Plan.Context.ExpiresAt) {
				t.Fatal("source not bound to Grant")
			}
			journal := store.NewMemory()
			customerSource := fixture.Source(t, journal, journal)
			if scenario == "provider failure before Runner" {
				fixture.Provider.Failure.Store(true)
				fixture.Publish(t, 2)
			}
			if scenario == "source expired" {
				customerSource.Now = func() time.Time { return run.Plan.Context.ExpiresAt.Add(time.Second) }
			}
			if scenario == "untrusted metadata revision" {
				for key, metadata := range customerSource.Trust.Metadata {
					metadata.Revision = strings.Repeat("c", 40)
					customerSource.Trust.Metadata[key] = metadata
				}
			}
			manifestSigner, manifestKey, _ := grant.NewDevelopmentSigner("manifest-key")
			manifest, err := ApprovedManifest(ctx, memory, manifestSigner, "https://approval.example", request, 1, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			manifests := &runnercore.ManifestContexts{Store: journal, Issuer: manifest.Issuer, TenantID: fixture.TenantID, RunnerGroup: record.Grant.RunnerGroup, Keys: grant.StaticKeys{manifest.Issuer + "\x00manifest-key": manifestKey}}
			if err := manifests.Pin(ctx, manifest); err != nil {
				t.Fatal(err)
			}
			operations := &sourceOperations{tenant: fixture.TenantID, observationFlowSource: &observationFlowSource{now: time.Now().UTC()}}
			if scenario == "provider failure during credentials" {
				operations.before = func() { fixture.Provider.Failure.Store(true); fixture.Publish(t, 2) }
			}
			runner := &runnercore.Runner{RunnerID: "runner", TenantID: fixture.TenantID, RunnerGroup: record.Grant.RunnerGroup, Grants: &grant.Verifier{Issuer: issuer.Issuer, TenantID: fixture.TenantID, RunnerGroup: record.Grant.RunnerGroup, Keys: grant.StaticKeys{issuer.Issuer + "\x00grant-key": key}}, Subjects: observationFlowSubject{time.Now().UTC()}, Delegations: &identity.DelegationVerifier{Resolver: identity.StaticDelegations{"delegation-1": {ID: "delegation-1", Issuer: "https://idp.example", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"production"}, MaximumRisk: "critical", ExpiresAt: time.Now().Add(time.Hour)}}}, Sources: &SourceAuthority{Source: customerSource}, Contexts: manifests, Approvals: manifests, Capabilities: runnercore.NewCapabilitySet(protocol.CapabilityDeploy), Policy: engine, Journal: journal, Credentials: operations, Adapter: operations, Connectivity: runnercore.ConnectionState(true)}
			result, err := runner.Execute(ctx, record.Grant)
			if scenario == "valid" {
				if err != nil || result.Status != protocol.ResultSucceeded || operations.writes.Load() != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else {
				if err == nil || operations.writes.Load() != 0 {
					t.Fatalf("source failure wrote: result=%+v err=%v", result, err)
				}
				if scenario != "provider failure during credentials" && operations.credentials != 0 {
					t.Fatal("invalid Context obtained deployment credentials")
				}
			}
			events, _ := memory.AuditEvents("context/" + fixture.TenantID + "/" + run.Plan.Context.Hash)
			raw, _ := json.Marshal(struct {
				Grant  any
				Events any
			}{record.Grant, events})
			if strings.Contains(string(raw), "source-credential-canary") {
				t.Fatal("source credential persisted or dispatched")
			}
			if scenario == "valid" {
				if err := os.Remove(fixture.Config.File); err != nil {
					t.Fatal(err)
				}
				if _, err := authority.VerifyPlan(ctx, run.Plan, run.TenantID); err == nil {
					t.Fatal("missing source reused cached PASS")
				}
			}
		})
	}
}

type sourceDispatchRaceStore struct {
	sourceDurableStore
	before func()
}

func (s sourceDispatchRaceStore) CreateGrantDispatch(ctx context.Context, record store.GrantDispatchRecord, audit domain.AuditEvent, outbox domain.OutboxEvent) (store.GrantDispatchRecord, bool, error) {
	s.before()
	return s.sourceDurableStore.CreateGrantDispatch(ctx, record, audit, outbox)
}
