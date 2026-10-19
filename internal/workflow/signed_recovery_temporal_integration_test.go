//go:build integration

package workflow_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/resultreport"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/internal/transport"
	wf "themisy/internal/workflow"
	"themisy/pkg/protocol"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Bound only in this test: shorten the initial result wait without changing the
// Workflow's production Activity timeout or granting permission to retry a Write.
type shortGrantWaitExecutor struct {
	executor.ReleaseExecutor
	calls atomic.Int32
}

func (e *shortGrantWaitExecutor) Deploy(ctx context.Context, input executor.DeployRequest) (executor.Deployment, error) {
	e.calls.Add(1)
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return e.ReleaseExecutor.Deploy(ctx, input)
}

func TestTemporalSignedGrantRecoveryAcrossWorkers(t *testing.T) {
	url, address := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL"), os.Getenv("THEMISY_TEMPORAL_ADDRESS")
	if url == "" || address == "" {
		t.Skip("requires explicitly dedicated PostgreSQL and Temporal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tc, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	nowFixture := time.Now().UTC()
	run := domain.ReleaseRun{ID: "run", WorkflowID: "run", RequestID: "request", ReleaseVersion: "v1", Environment: domain.EnvironmentStaging, RequestedBy: "requester", Agent: domain.AgentIdentity{ID: "agent", Scopes: []string{"release:deploy"}}, Plan: domain.ReleasePlan{Environment: domain.EnvironmentStaging, Phases: []domain.PlanPhase{{Number: 0, Steps: []domain.PlanStep{{Service: "identity", VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic}}}}}, Status: domain.RunPending, Steps: []domain.ReleaseStep{{Service: "identity", Status: domain.StepPending, Change: domain.Change{Service: "identity", CISuccess: true, DependenciesHealthy: true}, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic}}, StateVersion: 1, CreatedAt: nowFixture, UpdatedAt: nowFixture}
	verification := executor.NewMock(nil)
	run.ID += "-signed-recovery-" + time.Now().UTC().Format("20060102150405.000000000")
	run.WorkflowID, run.RequestID = run.ID, run.ID
	run.TenantID, run.SubjectType, run.UserIdentityProof, run.DelegationRef = "test-tenant", "user", "synthetic-identity", "synthetic-delegation"
	digest := "sha256:" + strings.Repeat("a", 64)
	run.Steps[0].Change.DesiredVersion = digest
	run.Plan.Hash, run.Plan.PlanHash, run.Plan.PolicyHash, run.Plan.EvidenceHash = digest, digest, digest, digest
	planned := &run.Plan.Phases[0].Steps[0]
	planned.DesiredVersion, planned.ContractHash, planned.ProfileHash = digest, digest, digest
	planned.Scheduling = domain.SchedulingContext{TenantID: run.TenantID, Environment: run.Environment, RunnerGroup: "test-group", RiskTier: "low"}
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	grantSigner, _, err := grant.NewDevelopmentSigner("grant-key")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := application.NewGrants(st, grantSigner, "https://control.example", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	initial := &shortGrantWaitExecutor{ReleaseExecutor: &application.GrantExecutor{Grants: issuer, Verification: verification, PollInterval: 10 * time.Millisecond}}
	ready := make(chan struct{}, 1)
	firstActivities := wf.NewActivities(st, policy.New(), initial)
	startWorker := func(a *wf.Activities, dropCompletion bool) worker.Worker {
		w := worker.New(tc, run.ID, worker.Options{})
		w.RegisterWorkflow(wf.ReleaseWorkflow)
		w.RegisterActivity(a.PersistRun)
		w.RegisterActivity(a.EvaluateStep)
		w.RegisterActivity(a.AcquireSchedule)
		w.RegisterActivity(a.CompleteSchedule)
		w.RegisterActivity(a.Deploy)
		w.RegisterActivity(a.Rollback)
		w.RegisterActivity(a.Verify)
		if dropCompletion {
			w.RegisterActivityWithOptions(func(ctx context.Context, in wf.RecoverExecutionInput) (wf.RecoverExecutionResult, error) {
				result, err := a.RecoverExecution(ctx, in)
				if err != nil {
					return result, err
				}
				if !result.Supported || result.Ready {
					return result, nil
				}
				select {
				case ready <- struct{}{}:
				default:
				}
				// The server must time out this lost completion, then retry read-only
				// recovery on the replacement Worker using unchanged production options.
				return wf.RecoverExecutionResult{}, activity.ErrResultPending
			}, activity.RegisterOptions{Name: wf.ActivityRecoverExecution})
		} else {
			w.RegisterActivity(a.RecoverExecution)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	first := startWorker(firstActivities, true)
	stopped := false
	defer func() {
		if !stopped {
			first.Stop()
		}
	}()
	execution, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: run.ID, TaskQueue: run.ID}, wf.ReleaseWorkflow, wf.ReleaseInput{Run: run})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	key := run.ID + "/staging/identity/" + digest
	unresolved, err := st.GetExecution("mock", key)
	if err != nil {
		t.Fatal(err)
	}
	grantID, ok := unresolved.Payload["grant_id"].(string)
	if !ok || grantID == "" || unresolved.Status != store.ExecutionUnknown {
		t.Fatalf("missing durable Grant: %+v", unresolved)
	}
	dispatch, err := st.GetGrantDispatch(ctx, grantID)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := protocol.GrantHash(dispatch.Grant)
	if err != nil {
		t.Fatal(err)
	}
	identity := resultreport.Identity{RunnerID: "test-runner", TenantID: run.TenantID, RunnerGroup: "test-group"}
	resultSigner, public, err := grant.NewDevelopmentSigner("result-key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	trust := resultreport.ResultTrustBundle{Version: "themisy.result-trust/v1", RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []resultreport.ResultTrustKey{{KeyID: "result-key", PublicKey: base64.RawURLEncoding.EncodeToString(public), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
	raw, err := json.Marshal(trust)
	if err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(trustPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&transport.RunnerServer{Store: st, Audit: st, ResultVerifier: &resultreport.Verifier{Keys: resultreport.FileKeys{Path: trustPath, Identity: identity}}, Auth: transport.StaticRunnerAuthenticator{identity.RunnerID: {TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, Token: "synthetic-token"}}}).Handler())
	defer server.Close()
	runner := &transport.RunnerClient{BaseURL: server.URL, RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, Token: "synthetic-token", HTTP: server.Client(), ResultSigner: &resultreport.Signer{Signer: resultSigner, KeyID: "result-key"}}
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: "identity", Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: time.Now().UTC()}
	report := store.RunnerActionRecord{GrantID: grantID, RequestHash: hash, Result: result}
	if err := runner.ReportRecovery(ctx, report); err != nil {
		t.Fatal(err)
	}
	first.Stop()
	stopped = true
	replacement, err := postgresstore.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	forbidden := &forbidWriteExecutor{ReleaseExecutor: verification}
	second := startWorker(wf.NewActivities(replacement, policy.New(), forbidden), false)
	defer second.Stop()
	report.Result.Status, report.Result.ExternalExecutionID, report.Result.ReasonCode, report.Result.CompletedAt = protocol.ResultSucceeded, "synthetic-provider-execution", "", time.Now().UTC()
	wrongSigner, _, err := grant.NewDevelopmentSigner("result-key")
	if err != nil {
		t.Fatal(err)
	}
	wrong := *runner
	wrong.ResultSigner = &resultreport.Signer{Signer: wrongSigner, KeyID: "result-key"}
	if err := wrong.ReportRecovery(ctx, report); err == nil {
		t.Fatal("untrusted signed result accepted")
	}
	unchanged, err := replacement.GetGrantDispatch(ctx, grantID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != store.GrantDispatchUnknown {
		t.Fatal("rejected report changed UNKNOWN dispatch")
	}
	if err := runner.ReportRecovery(ctx, report); err != nil {
		t.Fatal(err)
	}
	var finished domain.ReleaseRun
	if err := execution.Get(ctx, &finished); err != nil {
		t.Fatal(err)
	}
	saved, err := replacement.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []domain.ReleaseRun{finished, *saved} {
		if value.Status != domain.RunSucceeded || value.Steps[0].Status != domain.StepSucceeded || value.Steps[0].Execution == nil || value.Steps[0].Execution.ID != unresolved.ID || value.Steps[0].Execution.ExternalExecutionID != report.Result.ExternalExecutionID || value.Steps[0].Verification == nil {
			t.Fatalf("recovery did not converge existing execution: %+v", value)
		}
	}
	if initial.calls.Load() != 1 || forbidden.calls.Load() != 0 {
		t.Fatalf("dispatch calls=%d replacement writes=%d", initial.calls.Load(), forbidden.calls.Load())
	}
	outbox, err := replacement.PendingOutboxByType("grant.dispatch.requested", 100)
	if err != nil {
		t.Fatal(err)
	}
	issued := 0
	for _, event := range outbox {
		if event.AggregateID == grantID {
			issued++
		} else {
			record, err := replacement.GetGrantDispatch(ctx, event.AggregateID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Grant.RunID == run.ID {
				t.Fatal("replacement Grant issued")
			}
		}
	}
	if issued != 1 {
		t.Fatalf("Grant issuance outbox count=%d", issued)
	}
	history := &historypb.History{}
	iterator := tc.GetWorkflowHistory(ctx, run.ID, execution.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(wf.ReleaseWorkflow)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}

// A replacement Worker may verify but must never dispatch another Write.
type forbidWriteExecutor struct {
	executor.ReleaseExecutor
	calls atomic.Int32
}

func (e *forbidWriteExecutor) Deploy(context.Context, executor.DeployRequest) (executor.Deployment, error) {
	e.calls.Add(1)
	return executor.Deployment{}, fmt.Errorf("unexpected deploy")
}
func (e *forbidWriteExecutor) Rollback(context.Context, executor.Deployment) (executor.Deployment, error) {
	e.calls.Add(1)
	return executor.Deployment{}, fmt.Errorf("unexpected rollback")
}
