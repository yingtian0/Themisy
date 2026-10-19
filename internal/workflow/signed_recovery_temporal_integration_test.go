//go:build integration

package workflow_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"themisy/pkg/adapter"
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
	calls           atomic.Int32
	rollbackCalls   atomic.Int32
	pendingRollback bool
}

func (e *shortGrantWaitExecutor) Deploy(ctx context.Context, input executor.DeployRequest) (executor.Deployment, error) {
	e.calls.Add(1)
	if e.pendingRollback {
		return e.ReleaseExecutor.Deploy(ctx, input)
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return e.ReleaseExecutor.Deploy(ctx, input)
}

func (e *shortGrantWaitExecutor) Rollback(ctx context.Context, input executor.Deployment) (executor.Deployment, error) {
	e.rollbackCalls.Add(1)
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return e.ReleaseExecutor.Rollback(ctx, input)
}

func TestTemporalSignedGrantRecoveryAcrossWorkers(t *testing.T) { testTemporalSignedRecovery(t, false) }
func TestTemporalSignedRollbackRecoveryAcrossWorkers(t *testing.T) {
	testTemporalSignedRecovery(t, true)
}

type recoveryTestWorker interface{ Stop() }

func testTemporalSignedRecovery(t *testing.T, rollback bool) {
	testTemporalSignedRecoveryWithProcesses(t, rollback, false)
}
func testTemporalSignedRecoveryWithProcesses(t *testing.T, rollback, separateProcesses bool) {
	testTemporalSignedRecoveryScenario(t, rollback, separateProcesses, false)
}

type signedRecoveryOptions struct{ rollback, separateProcesses, cancelBeforeRecovery, nextStepAfterExpiry bool }

func testTemporalSignedRecoveryScenario(t *testing.T, rollback, separateProcesses, cancelBeforeRecovery bool) {
	testTemporalSignedRecoveryOptions(t, signedRecoveryOptions{rollback: rollback, separateProcesses: separateProcesses, cancelBeforeRecovery: cancelBeforeRecovery})
}
func testTemporalSignedRecoveryOptions(t *testing.T, options signedRecoveryOptions) {
	rollback, separateProcesses, cancelBeforeRecovery := options.rollback, options.separateProcesses, options.cancelBeforeRecovery
	expiringContext := cancelBeforeRecovery || options.nextStepAfterExpiry
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
	var verification executor.ReleaseExecutor = executor.NewMock(nil)
	if rollback {
		verification = rollbackPathVerifier{ReleaseExecutor: verification, failed: executor.NewMock(map[string]executor.MockBehavior{"": {VerifyStatus: adapter.VerificationFail}})}
	}
	run.ID += "-signed-recovery-" + time.Now().UTC().Format("20060102150405.000000000")
	run.WorkflowID, run.RequestID = run.ID, run.ID
	if expiringContext {
		run.Plan.ExpiresAt = time.Now().UTC().Add(20 * time.Second)
	}
	run.TenantID, run.SubjectType, run.UserIdentityProof, run.DelegationRef = "test-tenant-"+run.ID, "user", "synthetic-identity", "synthetic-delegation"
	digest := "sha256:" + strings.Repeat("a", 64)
	run.Steps[0].Change.DesiredVersion = digest
	run.Plan.Hash, run.Plan.PlanHash, run.Plan.PolicyHash, run.Plan.EvidenceHash = digest, digest, digest, digest
	planned := &run.Plan.Phases[0].Steps[0]
	planned.DesiredVersion, planned.ContractHash, planned.ProfileHash = digest, digest, digest
	planned.Scheduling = domain.SchedulingContext{TenantID: run.TenantID, Environment: run.Environment, RunnerGroup: "test-group", RiskTier: "low"}
	if options.nextStepAfterExpiry {
		next := *planned
		next.Service = "payments"
		run.Plan.Phases[0].Steps = append(run.Plan.Phases[0].Steps, next)
		nextStep := run.Steps[0]
		nextStep.Service = "payments"
		nextStep.Change.Service = "payments"
		run.Steps = append(run.Steps, nextStep)
	}
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	grantSigner, _, err := grant.NewDevelopmentSigner("grant-key")
	if err != nil {
		t.Fatal(err)
	}
	grantTTL := 2 * time.Minute
	if expiringContext {
		grantTTL = 2 * time.Second
	}
	issuer, err := application.NewGrants(st, grantSigner, "https://control.example", grantTTL)
	if err != nil {
		t.Fatal(err)
	}
	initial := &shortGrantWaitExecutor{pendingRollback: rollback, ReleaseExecutor: &application.GrantExecutor{Grants: issuer, Verification: verification, PollInterval: 10 * time.Millisecond}}
	ready := make(chan struct{}, 1)
	firstActivities := wf.NewActivities(st, policy.New(), initial)
	startWorker := func(a *wf.Activities, dropCompletion bool) recoveryTestWorker {
		if separateProcesses {
			return startRecoveryWorkerProcess(t, ctx, run.ID, rollback, dropCompletion, ready)
		}
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
	if rollback {
		// Supply the initial deploy's signed success. Its FAIL verification then
		// authorizes the already-configured automatic rollback through Grants.Issue.
		var deployed store.GrantDispatchRecord
		for {
			deployed, err = st.ClaimGrantDispatch(ctx, run.TenantID, identity.RunnerGroup, identity.RunnerID, "synthetic-delivery", time.Now().UTC(), time.Now().UTC().Add(time.Minute))
			if err == nil {
				break
			}
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if _, err := st.AcknowledgeGrantDispatch(ctx, deployed.Grant.GrantID, identity.RunnerID, deployed.DeliveryToken, time.Now().UTC(), time.Now().UTC().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		hash, err := protocol.GrantHash(deployed.Grant)
		if err != nil {
			t.Fatal(err)
		}
		if err := runner.ReportRecovery(ctx, store.RunnerActionRecord{GrantID: deployed.Grant.GrantID, RequestHash: hash, Result: protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: deployed.Grant.GrantID, RunID: run.ID, StepID: "identity", Status: protocol.ResultSucceeded, ExternalExecutionID: "initial-deploy", CompletedAt: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	key := run.ID + "/staging/identity/" + digest
	adapterName := "mock"
	if rollback {
		adapterName += ".rollback"
		key += "/rollback"
	}
	unresolved, err := st.GetExecution(adapterName, key)
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
	if rollback && dispatch.Grant.Action.ExternalExecutionID != "initial-deploy" {
		t.Fatal("rollback Grant lost durable source binding")
	}
	hash, err := protocol.GrantHash(dispatch.Grant)
	if err != nil {
		t.Fatal(err)
	}
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
	if expiringContext {
		// The child issues a normal short-lived Grant bounded by Plan expiry.
		// Wait until it has expired; observing its old Write remains permitted.
		timer := time.NewTimer(time.Until(dispatch.Grant.ExpiresAt) + time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		}
	}
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
	if cancelBeforeRecovery {
		if err := tc.SignalWorkflow(ctx, run.ID, execution.GetRunID(), wf.SignalCancel, wf.ControlSignal{Actor: "operator"}); err != nil {
			t.Fatal(err)
		}
	}
	var finished domain.ReleaseRun
	if err := execution.Get(ctx, &finished); err != nil {
		t.Fatal(err)
	}
	saved, err := replacement.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRun, wantStep := domain.RunSucceeded, domain.StepSucceeded
	wantRollbacks := int32(0)
	if rollback {
		wantRun, wantStep, wantRollbacks = domain.RunFailed, domain.StepRolledBack, 1
	}
	if cancelBeforeRecovery {
		wantRun, wantStep = domain.RunCancelled, domain.StepUnknown
	}
	if options.nextStepAfterExpiry {
		wantRun = domain.RunBlocked
	}
	for _, value := range []domain.ReleaseRun{finished, *saved} {
		if options.nextStepAfterExpiry && (len(value.Steps) != 2 || value.Steps[1].Status != domain.StepUnknown) {
			t.Fatalf("expired downstream context advanced: %+v", value)
		}
		if cancelBeforeRecovery {
			if value.Status != wantRun || value.Steps[0].Status != wantStep || value.Steps[0].Verification != nil {
				t.Fatalf("late recovery overrode cancellation: %+v", value)
			}
			continue
		}
		executed, verified := value.Steps[0].Execution, value.Steps[0].Verification
		if rollback {
			executed, verified = value.Steps[0].RollbackExecution, value.Steps[0].RollbackVerification
		}
		if value.Status != wantRun || value.Steps[0].Status != wantStep || executed == nil || executed.ID != unresolved.ID || executed.ExternalExecutionID != report.Result.ExternalExecutionID || verified == nil || verified.Status != domain.VerificationPass {
			t.Fatalf("recovery did not converge existing execution: %+v", value)
		}
	}
	if !separateProcesses && (initial.calls.Load() != 1 || initial.rollbackCalls.Load() != wantRollbacks || forbidden.calls.Load() != 0) {
		t.Fatalf("deploy calls=%d rollback calls=%d replacement writes=%d", initial.calls.Load(), initial.rollbackCalls.Load(), forbidden.calls.Load())
	}
	if cancelBeforeRecovery {
		if !time.Now().After(run.Plan.ExpiresAt) || !time.Now().After(dispatch.Grant.ExpiresAt) {
			t.Fatal("test did not cross Plan and Grant expiry")
		}
		activity := wf.NewActivities(replacement, policy.New(), forbidden)
		if _, err := activity.RecoverExecution(ctx, wf.RecoverExecutionInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: digest, IdempotencyKey: key}); err == nil {
			t.Fatal("terminal cancelled Run accepted delayed recovery")
		}
		after, err := replacement.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != domain.RunCancelled || after.Steps[0].Status != domain.StepUnknown {
			t.Fatal("delayed recovery mutated cancellation")
		}
	}
	if options.nextStepAfterExpiry {
		if !time.Now().After(run.Plan.ExpiresAt) || !time.Now().After(dispatch.Grant.ExpiresAt) {
			t.Fatal("recovery did not cross expiry")
		}
		next, err := replacement.GetExecution("mock", run.ID+"/staging/payments/"+digest)
		if err != nil {
			t.Fatal(err)
		}
		if next.Status != store.ExecutionUnknown || next.Payload["grant_id"] != nil {
			t.Fatal("expired downstream step obtained a Grant or advanced")
		}
	}
	// Claiming the initial deploy consumes its dispatch outbox row. Count
	// durable issuance audits, not just still-pending delivery notifications.
	audits, err := replacement.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[protocol.Capability]int{}
	expiryDenied := false
	for _, event := range audits {
		if event.Action == "deployment.result" && event.ResourceID == "payments" && strings.Contains(fmt.Sprint(event.Details["error"]), "plan or grant expired") {
			expiryDenied = true
		}
		if event.Action == "grant.issue" {
			counts[protocol.Capability(fmt.Sprint(event.Details["capability"]))]++
		}
	}
	if options.nextStepAfterExpiry && !expiryDenied {
		t.Fatal("downstream rejection was not caused by Plan expiry")
	}
	if counts[protocol.CapabilityDeploy] != 1 || counts[protocol.CapabilityRollback] != int(wantRollbacks) {
		t.Fatalf("unexpected Grant issuance counts: %+v", counts)
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

// Observations are synthetic but still pass the real Activities.Verify evidence
// validation. Fail the deploy, then pass the separately restored rollback.
type rollbackPathVerifier struct {
	executor.ReleaseExecutor
	failed executor.ReleaseExecutor
}

func (v rollbackPathVerifier) Verify(ctx context.Context, d executor.Deployment) (executor.VerificationResult, error) {
	if strings.HasSuffix(d.IdempotencyKey, "/rollback") {
		return v.ReleaseExecutor.Verify(ctx, d)
	}
	return v.failed.Verify(ctx, d)
}
