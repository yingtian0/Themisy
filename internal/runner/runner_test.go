package runner

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

func TestRunnerRejectsEveryUnsafeGrantBeforeAdapter(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *runnerFixture)
	}{
		{"expired grant", func(t *testing.T, f *runnerFixture) { f.grant.ExpiresAt = f.now.Add(-time.Second); f.resign(t) }},
		{"modified target", func(_ *testing.T, f *runnerFixture) { f.grant.Target.Service = "payments" }},
		{"modified artifact", func(_ *testing.T, f *runnerFixture) { f.grant.Action.ArtifactDigest = testDigest("b") }},
		{"re-signed artifact outside pinned plan", func(t *testing.T, f *runnerFixture) {
			f.grant.Action.ArtifactDigest = testDigest("b")
			f.resignWithPolicyHash(t)
		}},
		{"wrong audience", func(t *testing.T, f *runnerFixture) { f.grant.RunnerGroup = "other"; f.resign(t) }},
		{"wrong tenant", func(t *testing.T, f *runnerFixture) { f.grant.TenantID = "other"; f.resign(t) }},
		{"target outside Runner allowlist", func(_ *testing.T, f *runnerFixture) { f.adapter.authorizeErr = errors.New("target denied") }},
		{"reused nonce", func(t *testing.T, f *runnerFixture) {
			hash, _ := protocol.GrantHash(f.grant)
			record := store.RunnerActionRecord{GrantID: f.grant.GrantID, RunID: f.grant.RunID, StepID: f.grant.StepID, TenantID: f.grant.TenantID, RunnerGroup: f.grant.RunnerGroup, Nonce: f.grant.Nonce, IdempotencyKey: f.grant.IdempotencyKey, RequestHash: hash, CreatedAt: f.now, UpdatedAt: f.now}
			if _, created, err := f.journal.ReserveRunnerAction(context.Background(), record, testAudit(f.now)); err != nil || !created {
				t.Fatalf("seed replay: %v", err)
			}
		}},
		{"missing user proof", func(_ *testing.T, f *runnerFixture) { f.grant.UserIdentityProof = "" }},
		{"agent scope insufficient", func(_ *testing.T, f *runnerFixture) { f.delegation.Actions = nil; f.refreshDelegation() }},
		{"production outside delegation", func(t *testing.T, f *runnerFixture) {
			f.grant.Target.Environment = "production"
			f.resignWithPolicyHash(t)
			f.delegation.Environments = []string{"staging"}
			f.refreshDelegation()
		}},
		{"hash mismatch", func(t *testing.T, f *runnerFixture) { f.grant.PlanHash = testDigest("c"); f.resignWithPolicyHash(t) }},
		{"missing approval", func(t *testing.T, f *runnerFixture) { f.grant.ApprovalProofs = nil; f.resignWithPolicyHash(t) }},
		{"unsupported capability", func(t *testing.T, f *runnerFixture) {
			f.grant.Action.Capability = protocol.CapabilityRollback
			f.grant.Action.ExternalExecutionID = "external-original"
			f.syncPinnedTargetAction()
			f.resignWithPolicyHash(t)
		}},
		{"control plane allow runner deny", func(_ *testing.T, f *runnerFixture) { f.policy.deny = true }},
		{"policy mismatch", func(t *testing.T, f *runnerFixture) { f.grant.PolicyInputHash = testDigest("d"); f.resign(t) }},
		{"audit journal unavailable", func(_ *testing.T, f *runnerFixture) { f.journal.SetRunnerJournalError(store.ErrJournalUnavailable) }},
		{"control plane disconnect", func(_ *testing.T, f *runnerFixture) { f.runner.Connectivity = ConnectionState(false) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			test.prepare(t, fixture)
			if _, err := fixture.runner.Execute(context.Background(), fixture.grant); err == nil {
				t.Fatal("unsafe grant was accepted")
			}
			if calls := fixture.adapter.Calls(); calls != 0 {
				t.Fatalf("adapter calls=%d, want 0", calls)
			}
			if calls := fixture.credentials.Calls(); calls != 0 {
				t.Fatalf("credential calls=%d, want 0", calls)
			}
		})
	}
}

func TestRunnerPersistsNonceBeforeWriteAndReplayReturnsRecordedResult(t *testing.T) {
	fixture := newRunnerFixture(t)
	result, err := fixture.runner.Execute(context.Background(), fixture.grant)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.ResultSucceeded || fixture.adapter.Calls() != 1 || fixture.credentials.Calls() != 1 {
		t.Fatalf("result=%#v adapter=%d credential=%d", result, fixture.adapter.Calls(), fixture.credentials.Calls())
	}
	restarted := *fixture.runner
	result, err = restarted.Execute(context.Background(), fixture.grant)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.ResultSucceeded || fixture.adapter.Calls() != 1 {
		t.Fatalf("replay result=%#v calls=%d", result, fixture.adapter.Calls())
	}
}

func TestConcurrentDuplicateGrantCreatesAtMostOneWrite(t *testing.T) {
	fixture := newRunnerFixture(t)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _ = fixture.runner.Execute(context.Background(), fixture.grant)
		}()
	}
	close(start)
	wait.Wait()
	if fixture.adapter.Calls() != 1 || fixture.credentials.Calls() != 1 {
		t.Fatalf("adapter=%d credential=%d, want one", fixture.adapter.Calls(), fixture.credentials.Calls())
	}
}

func TestDisconnectedRunnerMayReconcileButCannotStartWrite(t *testing.T) {
	fixture := newRunnerFixture(t)
	hash, _ := protocol.GrantHash(fixture.grant)
	record := store.RunnerActionRecord{GrantID: fixture.grant.GrantID, RunID: fixture.grant.RunID, StepID: fixture.grant.StepID, TenantID: fixture.grant.TenantID, RunnerGroup: fixture.grant.RunnerGroup, Nonce: fixture.grant.Nonce, IdempotencyKey: fixture.grant.IdempotencyKey, RequestHash: hash, Target: fixture.grant.Target, Action: fixture.grant.Action, CreatedAt: fixture.now, UpdatedAt: fixture.now}
	reserved, _, err := fixture.journal.ReserveRunnerAction(context.Background(), record, testAudit(fixture.now))
	if err != nil {
		t.Fatal(err)
	}
	reserved.Status = store.RunnerActionUnknown
	reserved.UpdatedAt = fixture.now.Add(time.Second)
	if err := fixture.journal.CompleteRunnerAction(context.Background(), reserved, reserved.StateVersion, domain.AuditEvent{ID: "unknown-audit", CorrelationID: fixture.grant.GrantID, Timestamp: fixture.now}); err != nil {
		t.Fatal(err)
	}
	fixture.runner.Connectivity = ConnectionState(false)
	if err := fixture.runner.Reconcile(context.Background(), staticReconciler{result: AdapterResult{ExternalExecutionID: "external-reconciled", CompletedAt: fixture.now}}, 10); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.journal.GetRunnerAction(context.Background(), fixture.grant.TenantID, fixture.grant.RunnerGroup, fixture.grant.Nonce)
	if err != nil || loaded.Status != store.RunnerActionSucceeded {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if fixture.adapter.Calls() != 0 || fixture.credentials.Calls() != 1 {
		t.Fatalf("reconcile started a write or failed to obtain one read credential")
	}
}

type runnerFixture struct {
	now         time.Time
	grant       protocol.ActionGrant
	signer      grant.Signer
	public      ed25519.PublicKey
	delegation  identity.Delegation
	journal     *store.Memory
	policy      *staticPolicy
	adapter     *countingAdapter
	credentials *countingCredentials
	runner      *Runner
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	signer, public, err := grant.NewDevelopmentSigner("grant-key")
	if err != nil {
		t.Fatal(err)
	}
	d := testDigest("a")
	actionGrant := protocol.ActionGrant{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: "grant-1", Issuer: "https://control.example", TenantID: "tenant-1", RunnerGroup: "prod-jp", RunID: "run-1", StepID: "step-1", SubjectType: "user", UserSubject: "user-1", UserIdentityProof: "verified-token", AgentID: "agent-1", DelegationRef: "delegation-1", Target: protocol.Target{Service: "identity", Environment: "staging"}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: d}, Risk: "medium", PlanHash: d, ContractHash: d, ProfileHash: d, PolicyHash: d, EvidenceHash: d, ApprovalProofs: []string{"approval-1"}, IdempotencyKey: "key-1", Nonce: "nonce-1", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
	delegation := identity.Delegation{ID: "delegation-1", Issuer: "https://idp.example", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy, protocol.CapabilityRollback}, ServiceSelectors: []string{"identity"}, Environments: []string{"staging"}, MaximumRisk: "medium", ExpiresAt: now.Add(time.Hour)}
	policyEvaluator := &staticPolicy{policyHash: d}
	fixture := &runnerFixture{now: now, grant: actionGrant, signer: signer, public: public, delegation: delegation, journal: store.NewMemory(), policy: policyEvaluator, adapter: &countingAdapter{now: now}, credentials: &countingCredentials{now: now}}
	fixture.runner = &Runner{RunnerGroup: "prod-jp", Grants: &grant.Verifier{Issuer: actionGrant.Issuer, RunnerGroup: actionGrant.RunnerGroup, TenantID: actionGrant.TenantID, Keys: grant.StaticKeys{actionGrant.Issuer + "\x00grant-key": public}, Now: func() time.Time { return now }}, Subjects: staticSubjectVerifier{subject: identity.Subject{Issuer: "https://idp.example", ID: "user-1", ExpiresAt: now.Add(time.Hour)}}, Contexts: StaticContexts{actionGrant.RunID + "\x00" + actionGrant.StepID: PinnedContext{PlanHash: d, ContractHash: d, ProfileHash: d, PolicyHash: d, EvidenceHash: d, ApprovalRequired: true, Target: actionGrant.Target, Action: actionGrant.Action}}, Approvals: ProofReferences{"approval-1": {}}, Capabilities: NewCapabilitySet(protocol.CapabilityDeploy), Policy: policy.NewEngine(policyEvaluator), Journal: fixture.journal, Credentials: fixture.credentials, Adapter: fixture.adapter, Connectivity: ConnectionState(true), Now: func() time.Time { return now }}
	fixture.runner.TenantID = actionGrant.TenantID
	fixture.refreshDelegation()
	fixture.resignWithPolicyHash(t)
	return fixture
}

func (f *runnerFixture) refreshDelegation() {
	f.runner.Delegations = &identity.DelegationVerifier{Resolver: identity.StaticDelegations{f.delegation.ID: f.delegation}, Now: func() time.Time { return f.now }}
}
func (f *runnerFixture) syncPinnedTargetAction() {
	contexts := f.runner.Contexts.(StaticContexts)
	key := f.grant.RunID + "\x00" + f.grant.StepID
	pinned := contexts[key]
	pinned.Target, pinned.Action = f.grant.Target, f.grant.Action
	contexts[key] = pinned
}
func (f *runnerFixture) resign(t *testing.T) {
	t.Helper()
	signed, err := grant.SignActionGrant(context.Background(), f.signer, f.grant)
	if err != nil {
		t.Fatal(err)
	}
	f.grant = signed
}
func (f *runnerFixture) resignWithPolicyHash(t *testing.T) {
	t.Helper()
	input := policy.InputForGrant(f.grant, "https://idp.example", "user-1", f.delegation.AgentID, f.delegation.ID)
	f.grant.PolicyInputHash, _ = policy.InputHash(input)
	f.resign(t)
}

type staticSubjectVerifier struct {
	subject identity.Subject
	err     error
}

func (s staticSubjectVerifier) Verify(_ context.Context, _ string) (identity.Subject, error) {
	return s.subject, s.err
}

type staticPolicy struct {
	policyHash string
	deny       bool
}

func (p *staticPolicy) Evaluate(_ context.Context, input policy.Input) (policy.Evaluation, error) {
	hash, _ := policy.InputHash(input)
	decision := domain.DecisionAllow
	if p.deny {
		decision = domain.DecisionDeny
	}
	return policy.Evaluation{Decision: decision, InputHash: hash, PolicyHash: p.policyHash, PolicyVersion: "test"}, nil
}

type countingAdapter struct {
	mu           sync.Mutex
	calls        int
	now          time.Time
	authorizeErr error
}

func (a *countingAdapter) Authorize(protocol.Target, protocol.Action) error { return a.authorizeErr }

func (a *countingAdapter) Execute(_ context.Context, _ AdapterRequest, _ Credential) (AdapterResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return AdapterResult{ExternalExecutionID: "external-1", CompletedAt: a.now}, nil
}
func (a *countingAdapter) Calls() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

type countingCredentials struct {
	mu    sync.Mutex
	calls int
	now   time.Time
}

type staticReconciler struct{ result AdapterResult }

func (r staticReconciler) Reconcile(context.Context, AdapterRequest, Credential) (AdapterResult, bool, error) {
	return r.result, true, nil
}

func (b *countingCredentials) Acquire(_ context.Context, _ CredentialRequest) (Credential, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return credentials.New(map[string][]byte{"token": []byte("redacted")}, b.now.Add(time.Minute)), nil
}
func (b *countingCredentials) Calls() int { b.mu.Lock(); defer b.mu.Unlock(); return b.calls }

func testDigest(value string) string { return "sha256:" + strings.Repeat(value, 64) }
func testAudit(now time.Time) domain.AuditEvent {
	return domain.AuditEvent{ID: "audit-seed", CorrelationID: "grant-1", ActorType: "runner", ActorID: "test", Action: "reserve", ResourceType: "grant", ResourceID: "grant-1", Result: "AUTHORIZED", Timestamp: now}
}
