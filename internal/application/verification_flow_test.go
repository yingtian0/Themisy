package application

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"themisy/adapters/datadog"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/internal/resultreport"
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	"themisy/internal/transport"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
	"time"
)

type observationFlowSource struct {
	now    time.Time
	writes atomic.Int32
}

func (s *observationFlowSource) Authorize(t protocol.Target, a protocol.Action) error {
	if t.Service != "payment-api" || t.Environment != "production" {
		return fmt.Errorf("target refused")
	}
	return nil
}
func (s *observationFlowSource) Execute(_ context.Context, r runnercore.AdapterRequest, _ credentials.Credential) (runnercore.AdapterResult, error) {
	s.writes.Add(1)
	return runnercore.AdapterResult{ExternalExecutionID: "external-" + string(r.Action.Capability), CompletedAt: time.Now().UTC()}, nil
}
func (s *observationFlowSource) Acquire(_ context.Context, r credentials.Request) (credentials.Credential, error) {
	if r.TenantID != "tenant-1" || r.Service != "payment-api" {
		return credentials.Credential{}, fmt.Errorf("scope refused")
	}
	return credentials.New(map[string][]byte{"fixture": []byte("read-write-scoped"), "api_key": []byte("fixture-api"), "app_key": []byte("fixture-app")}, s.now.Add(time.Hour)), nil
}
func (s *observationFlowSource) ReadRuntime(_ context.Context, r adapter.ReconcileRequest, _ credentials.Credential) (adapter.RuntimeObservation, error) {
	hash, _ := adapter.RuntimeRequestHash(r)
	v := adapter.RuntimeObservation{SchemaVersion: "themisy.ecs-runtime/v1alpha1", Target: r.Target, Operation: r.Operation, RequestHash: hash, DefinitionHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContainerSetHash: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: adapter.VerificationPass, ReasonCode: "RUNTIME_CONFIRMED", ObservedAt: time.Now().UTC(), AdapterVersion: "fixture/v1", Summary: adapter.RuntimeSummary{DesiredCount: 1, RunningCount: 1, TaskCount: 1, RolloutState: "COMPLETED"}}
	v.ContainerDigests = map[string]string{"app": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	v.ContainerSetHash, _ = adapter.ContainerSetHash(v.ContainerDigests)
	v.Hash, _ = adapter.RuntimeObservationHash(v)
	return v, nil
}
func (s *observationFlowSource) Name() string    { return "fixture" }
func (s *observationFlowSource) Version() string { return "v1" }
func (s *observationFlowSource) Verify(_ context.Context, r adapter.VerificationRequest, _ credentials.Credential) (adapter.Evidence, error) {
	status := adapter.VerificationFail
	if r.Deployment.ExternalExecutionID == "external-release.rollback" {
		status = adapter.VerificationPass
	}
	e := adapter.Evidence{Status: status, ReasonCode: "HEALTH_" + string(status), Source: "fixture-health", QueryHash: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Window: r.Window, ObservedAt: time.Now().UTC(), AdapterVersion: "fixture/v1"}
	e.EvidenceHash, _ = adapter.EvidenceHash(e)
	return e, nil
}

type observationFlowSubject struct{ now time.Time }

func (s observationFlowSubject) Verify(context.Context, string) (identity.Subject, error) {
	return identity.Subject{Issuer: "https://idp.example", ID: "user-1", ExpiresAt: s.now.Add(time.Hour)}, nil
}

type observationFlowContext struct{ store store.DurableStore }

func (s observationFlowContext) ResolveContext(context.Context, string, string) (runnercore.PinnedContext, error) {
	return runnercore.PinnedContext{}, nil
}
func (s observationFlowContext) ResolveForGrant(_ context.Context, g protocol.ActionGrant) (runnercore.PinnedContext, error) {
	r, err := s.store.GetRun(g.RunID)
	if err != nil {
		return runnercore.PinnedContext{}, err
	}
	p := r.Plan.Phases[0].Steps[1]
	action := protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: r.Steps[1].Change.DesiredVersion}
	if r.Steps[1].Status == domain.StepRollingBack {
		action.Capability = protocol.CapabilityRollback
		action.ExternalExecutionID = r.Steps[1].Execution.ExternalExecutionID
	}
	return runnercore.PinnedContext{PlanHash: r.Plan.Hash, ContractHash: p.ContractHash, ProfileHash: p.ProfileHash, PolicyHash: r.Plan.PolicyHash, EvidenceHash: r.Plan.EvidenceHash, Target: protocol.Target{Service: "payment-api", Environment: string(r.Environment)}, Action: action, ApprovalRequired: r.Steps[1].Approval != nil}, nil
}

func TestSignedObservationFlowDeployFailureRollbackAndVerification(t *testing.T) {
	testObservationFlow(t, store.NewMemory(), store.NewMemory())
}
func testObservationFlow(t *testing.T, control store.DurableStore, journal store.RunnerJournal, options ...observationFlowOptions) observationFlowReceipt {
	t.Helper()
	now := time.Now().UTC()
	run := grantRun(now)
	run.ID = "observation-flow-" + fmt.Sprint(now.UnixNano())
	run.RequestID = run.ID
	run.WorkflowID = run.ID
	run.Plan.Phases[0].Steps[1].ObservationWindow = "2s"
	var option observationFlowOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.Prepare != nil {
		option.Prepare(run)
	}
	if _, _, err := control.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, key, err := grant.NewDevelopmentSigner("grant-key")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewGrants(control, signer, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return time.Now().UTC() }
	resultSigner, resultKey, err := grant.NewDevelopmentSigner("result-key")
	if err != nil {
		t.Fatal(err)
	}
	id := resultreport.Identity{RunnerID: "runner-1", TenantID: "tenant-1", RunnerGroup: "payments-prod"}
	server := &transport.RunnerServer{Store: control, Audit: control, Auth: transport.StaticRunnerAuthenticator{"runner-1": {TenantID: id.TenantID, RunnerGroup: id.RunnerGroup, Token: "fixture"}}, ResultVerifier: &resultreport.Verifier{Keys: resultreport.ScopedKeys{id: observationResultKey{resultKey}}}, Now: func() time.Time { return time.Now().UTC() }, MaxWait: time.Millisecond, PollInterval: time.Millisecond}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	source := &observationFlowSource{now: now}
	healthAdapter, err := datadog.New(datadog.Config{Site: datadog.SiteUS1, Queries: map[datadog.QueryKey]datadog.Query{{Service: "payment-api", Environment: "production"}: {Expression: "metric.error{service:payment-api}", Comparator: "lte", Threshold: 1, Aggregation: "last", MinimumPoints: 1, MaximumAge: time.Minute}}}, &observationMetricsHTTP{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.NewMandatoryEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var healthSource adapter.VerificationAdapter = healthAdapter
	if option.FixtureHealth {
		healthSource = source
	}
	runner := &runnercore.Runner{RunnerID: id.RunnerID, TenantID: id.TenantID, RunnerGroup: id.RunnerGroup, Grants: &grant.Verifier{Issuer: issuer.Issuer, TenantID: id.TenantID, RunnerGroup: id.RunnerGroup, Keys: grant.StaticKeys{issuer.Issuer + "\x00grant-key": key}, Now: func() time.Time { return time.Now().UTC() }}, Subjects: observationFlowSubject{now}, Delegations: &identity.DelegationVerifier{Resolver: identity.StaticDelegations{"delegation-1": {ID: "delegation-1", Issuer: "https://idp.example", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy, protocol.CapabilityRollback}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(time.Hour)}}, Now: func() time.Time { return time.Now().UTC() }}, Contexts: observationFlowContext{control}, Approvals: runnercore.ProofReferences{"approval-1": {}}, Capabilities: runnercore.NewCapabilitySet(protocol.CapabilityDeploy, protocol.CapabilityRollback, protocol.CapabilityVerify), Policy: engine, Journal: journal, Credentials: source, Adapter: source, Connectivity: runnercore.ConnectionState(true), Now: func() time.Time { return time.Now().UTC() }, Observations: &runnercore.ObservationVerifier{Runtime: source, RuntimeBroker: source, Health: healthSource, HealthBroker: source}}
	client := &transport.RunnerClient{BaseURL: httpServer.URL, RunnerID: id.RunnerID, TenantID: id.TenantID, RunnerGroup: id.RunnerGroup, Token: "fixture", HTTP: httpServer.Client(), Wait: time.Millisecond, ResultSigner: &resultreport.Signer{Signer: resultSigner, KeyID: "result-key"}}
	if option.Tamper {
		client.ResultSigner.Signer = observationTamperSigner{Signer: resultSigner}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		for i := 0; i < 10000; i++ {
			if ctx.Err() != nil {
				finished <- nil
				return
			}
			_, err := client.RunOnce(ctx, runner)
			if err != nil && ctx.Err() == nil && !option.Tamper {
				finished <- err
				return
			}
		}
		finished <- fmt.Errorf("delivery loop exhausted")
	}()
	defer func() {
		cancel()
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	exec := &GrantExecutor{Grants: issuer, PollInterval: time.Millisecond}
	if option.Drive != nil {
		option.Drive(run, exec, source)
		return observationFlowReceipt{RunID: run.ID, Key: resultKey}
	}
	deployed, err := exec.Deploy(ctx, executor.DeployRequest{RunID: run.ID, Service: "payment-api", Environment: "production", DesiredVersion: run.Steps[1].Change.DesiredVersion, IdempotencyKey: run.ID + "/deploy"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	observed, err := exec.Verify(ctx, deployed)
	if err != nil || observed.Status != adapter.VerificationFail || observed.Runtime == nil {
		t.Fatalf("deploy observation=%+v err=%v", observed, err)
	}
	current, err := control.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Steps[1].Status = domain.StepRollingBack
	current.Steps[1].Execution = &domain.Execution{ExternalExecutionID: deployed.ExternalID}
	if err := control.UpdateRun(current, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	rollback, err := exec.Rollback(ctx, deployed)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	observed, err = exec.Verify(ctx, rollback)
	if err != nil || observed.Status != adapter.VerificationPass {
		t.Fatalf("rollback observation=%+v err=%v", observed, err)
	}
	if source.writes.Load() != 2 {
		t.Fatalf("writes=%d", source.writes.Load())
	}
	events, err := control.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reports := 0
	for _, event := range events {
		if event.Action == "grant.result" {
			reports++
		}
	}
	if reports != 4 {
		t.Fatalf("signed persisted results=%d", reports)
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"fixture-api", "fixture-app", "read-write-scoped"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential leaked into observation audit")
		}
	}
	return observationFlowReceipt{RunID: run.ID, Key: resultKey}

}

type observationResultKey struct{ key ed25519.PublicKey }

func (k observationResultKey) ResolveResultKey(context.Context, resultreport.Identity, string) (ed25519.PublicKey, error) {
	return k.key, nil
}

type observationMetricsHTTP struct{ calls atomic.Int32 }

func (h *observationMetricsHTTP) Do(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.Host != "api.datadoghq.com" || r.Header.Get("DD-API-KEY") != "fixture-api" || r.Header.Get("DD-APPLICATION-KEY") != "fixture-app" {
		return nil, fmt.Errorf("unexpected metrics request")
	}
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if err != nil {
		return nil, err
	}
	value := 2
	if h.calls.Add(1) > 1 {
		value = 0
	}
	body := fmt.Sprintf(`{"status":"ok","series":[{"pointlist":[[%d,%d]]}]}`, to*1000, value)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

type observationFlowReceipt struct {
	RunID string
	Key   ed25519.PublicKey
}

type observationFlowOptions struct {
	Prepare       func(*domain.ReleaseRun)
	FixtureHealth bool
	Tamper        bool
	Drive         func(*domain.ReleaseRun, *GrantExecutor, *observationFlowSource)
}
