package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/scheduler"
	"themisy/internal/store"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestReleaseWorkflowCompletesThroughActivities(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	env := newWorkflowEnvironment(memory, exec)
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunSucceeded || result.Steps[0].Status != domain.StepSucceeded {
		t.Fatalf("run=%s step=%s", result.Status, result.Steps[0].Status)
	}
	if calls := exec.DeployCalls(result.Steps[0].Execution.IdempotencyKey); calls != 1 {
		t.Fatalf("deploy calls=%d, want 1", calls)
	}
	persisted, err := memory.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != domain.RunSucceeded {
		t.Fatalf("persisted status=%s", persisted.Status)
	}
}

func TestReleaseWorkflowDoesNotDispatchAtZeroRunnerCapacity(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := NewActivities(memory, policy.New(), exec)
	activities.Capacity = func(ScheduleInput) scheduler.Capacity {
		return scheduler.Capacity{RunnerAvailable: 0, AdapterRemaining: 10, QueueLimit: 10}
	}
	env.RegisterActivity(activities.PersistRun)
	env.RegisterActivity(activities.EvaluateStep)
	env.RegisterActivity(activities.Deploy)
	env.RegisterActivity(activities.Verify)
	env.RegisterActivity(activities.Rollback)
	env.RegisterActivity(activities.AcquireSchedule)
	env.RegisterActivity(activities.CompleteSchedule)
	env.RegisterActivity(activities.RecoverExecution)
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunBlocked || exec.DeployCalls(run.ID+"/staging/identity/sha-1") != 0 {
		t.Fatalf("result=%#v deploys=%d", result, exec.DeployCalls(run.ID+"/staging/identity/sha-1"))
	}
}

func TestApprovalPauseResumeAndCancelSignalsSurviveWorkflowWait(t *testing.T) {
	t.Run("approval after pause and resume", func(t *testing.T) {
		run, memory, exec := workflowFixture(t, true, nil)
		env := newWorkflowEnvironment(memory, exec)
		approvalID := deterministicID("approval", run.ID, run.Steps[0].Service)
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPause, ControlSignal{Actor: "operator"}) }, time.Minute)
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalResume, ControlSignal{Actor: "operator"}) }, 2*time.Minute)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalApproval, ApprovalSignal{ApprovalID: approvalID, Actor: "sre-user", Roles: []string{"service-owner", "sre"}, Approve: true})
		}, 3*time.Minute)
		env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		var result domain.ReleaseRun
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatal(err)
		}
		if result.Status != domain.RunSucceeded || result.Steps[0].Approval.Status != domain.ApprovalApproved {
			t.Fatalf("status=%s approval=%s", result.Status, result.Steps[0].Approval.Status)
		}
	})
	t.Run("cancel while waiting", func(t *testing.T) {
		run, memory, exec := workflowFixture(t, true, nil)
		env := newWorkflowEnvironment(memory, exec)
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalCancel, ControlSignal{Actor: "operator"}) }, time.Minute)
		env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		var result domain.ReleaseRun
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatal(err)
		}
		if result.Status != domain.RunCancelled || exec.DeployCalls(run.ID+"/production/identity/sha-1") != 0 {
			t.Fatalf("status=%s deploy should not run", result.Status)
		}
	})
}

func TestPersistActivityFailuresRecoverWithoutDuplicateWrite(t *testing.T) {
	for failAt := 1; failAt <= 5; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			run, memory, exec := workflowFixture(t, false, nil)
			flaky := &flakyUpdateStore{Memory: memory, failAt: failAt}
			env := newWorkflowEnvironment(flaky, exec)
			env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != domain.RunSucceeded {
				t.Fatalf("status=%s", result.Status)
			}
			if calls := exec.DeployCalls(run.ID + "/staging/identity/sha-1"); calls != 1 {
				t.Fatalf("deploy calls=%d", calls)
			}
		})
	}
}

func TestUnknownDeployStateNeverCreatesSecondWrite(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	failing := &failCompleteStore{Memory: memory, fail: true}
	activities := NewActivities(failing, policy.New(), exec)
	input := DeployInput{RunID: run.ID, RequestedBy: run.RequestedBy, AgentID: run.Agent.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "fixed-key"}
	if _, err := activities.Deploy(context.Background(), input); err == nil {
		t.Fatal("first deploy should report unknown persistence state")
	}
	if _, err := activities.Deploy(context.Background(), input); err == nil {
		t.Fatal("second deploy should require reconciliation")
	}
	if calls := exec.DeployCalls("fixed-key"); calls != 1 {
		t.Fatalf("deploy calls=%d, want 1", calls)
	}
}

func TestReleaseWorkflowWaitsForReadOnlyGrantRecovery(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	activities := NewActivities(memory, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: "grant-pending"})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(activities.PersistRun)
	env.RegisterActivity(activities.EvaluateStep)
	env.RegisterActivity(activities.Deploy)
	env.RegisterActivity(activities.Verify)
	env.RegisterActivity(activities.Rollback)
	env.RegisterActivity(activities.AcquireSchedule)
	env.RegisterActivity(activities.CompleteSchedule)
	recoveryCalls := 0
	env.RegisterActivityWithOptions(func(context.Context, RecoverExecutionInput) (RecoverExecutionResult, error) {
		recoveryCalls++
		if recoveryCalls <= 3 {
			return RecoverExecutionResult{}, errors.New("temporary recovery store outage")
		}
		return RecoverExecutionResult{Supported: true, Ready: true, Execution: domain.Execution{ID: "recovered-execution", Status: string(store.ExecutionSucceeded), ExternalExecutionID: "deployment-1"}, Deployment: executor.Deployment{RunID: run.ID, Service: "identity", Environment: string(run.Environment), DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/" + string(run.Environment) + "/identity/sha-1", ExternalID: "deployment-1", StartedAt: run.CreatedAt, FinishedAt: run.CreatedAt.Add(time.Second)}}, nil
	}, activity.RegisterOptions{Name: ActivityRecoverExecution})
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunSucceeded || recoveryCalls < 4 {
		t.Fatalf("workflow result=%s recovery calls=%d; want recovery after exhausted Activity retries", result.Status, recoveryCalls)
	}
}

func TestCancelDuringUnknownRecoveryKeepsStepAndReservationUnresolved(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	activities := NewActivities(memory, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: "grant-pending"})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(activities.PersistRun)
	env.RegisterActivity(activities.EvaluateStep)
	env.RegisterActivity(activities.Deploy)
	env.RegisterActivity(activities.Verify)
	env.RegisterActivity(activities.Rollback)
	env.RegisterActivity(activities.AcquireSchedule)
	env.RegisterActivity(activities.CompleteSchedule)
	env.RegisterActivityWithOptions(func(context.Context, RecoverExecutionInput) (RecoverExecutionResult, error) {
		return RecoverExecutionResult{Supported: true}, nil
	}, activity.RegisterOptions{Name: ActivityRecoverExecution})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalCancel, ControlSignal{Actor: "operator"}) }, time.Minute)
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunCancelled || result.Steps[0].Status != domain.StepUnknown {
		t.Fatalf("cancel overwrote unresolved step: run=%s step=%s", result.Status, result.Steps[0].Status)
	}
}

func TestRecoverExecutionKeepsTerminalUnknownRunQuarantined(t *testing.T) {
	for _, status := range []domain.RunStatus{domain.RunCancelled, domain.RunSucceeded, domain.RunFailed, domain.RunEscalated} {
		t.Run(string(status), func(t *testing.T) {
			run, memory, exec := workflowFixture(t, false, nil)
			run.Status = status
			run.Steps[0].Status = domain.StepUnknown
			if err := memory.UpdateRun(&run, 1); err != nil {
				t.Fatal(err)
			}
			activities := NewActivities(memory, policy.New(), exec)
			_, err := activities.RecoverExecution(context.Background(), RecoverExecutionInput{
				RunID: run.ID, Service: run.Steps[0].Service, Environment: string(run.Environment),
				DesiredVersion: run.Steps[0].Change.DesiredVersion, IdempotencyKey: run.ID + "/" + string(run.Environment) + "/identity/sha-1",
			})
			if err == nil {
				t.Fatal("terminal run must not be mutated by delayed recovery")
			}
			persisted, getErr := memory.GetRun(run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if persisted.Status != status || persisted.Steps[0].Status != domain.StepUnknown {
				t.Fatalf("recovery changed terminal state: run=%s step=%s", persisted.Status, persisted.Steps[0].Status)
			}
			if calls := exec.DeployCalls(run.ID + "/" + string(run.Environment) + "/identity/sha-1"); calls != 0 {
				t.Fatalf("recovery issued provider write: calls=%d", calls)
			}
		})
	}
}

func TestDurableCoordinatorUnavailableAndMissingLeaseFailClosed(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	coordinator := &coordinatorTestStore{Memory: memory, acquireErr: errors.New("database unavailable")}
	activities := NewActivities(coordinator, policy.New(), exec)
	input := ScheduleInput{RunID: run.ID, Step: scheduler.Step{ID: "identity", Tenant: run.TenantID, Environment: string(run.Environment), Region: run.Region, Cluster: run.Cluster}}
	result, err := activities.AcquireSchedule(context.Background(), input)
	if err != nil || result.Allowed || result.Reason != "SHARED_COORDINATOR_UNAVAILABLE" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := activities.Deploy(context.Background(), DeployInput{RunID: run.ID, RequestedBy: run.RequestedBy, AgentID: run.Agent.ID, Service: "identity", Environment: string(run.Environment), DesiredVersion: "sha-1", IdempotencyKey: "durable-missing-lease"}); err == nil {
		t.Fatal("deploy without durable lease should fail closed")
	}
	if calls := exec.DeployCalls("durable-missing-lease"); calls != 0 {
		t.Fatalf("provider calls=%d, want 0", calls)
	}
	if coordinator.quarantineCalls != 1 {
		t.Fatalf("quarantine calls=%d, want 1", coordinator.quarantineCalls)
	}
}

type coordinatorTestStore struct {
	*store.Memory
	acquireErr      error
	quarantineCalls int
}

func (s *coordinatorTestStore) AcquireSchedule(context.Context, scheduler.AcquireRequest) (scheduler.ScheduleLease, []scheduler.Blocked, scheduler.CircuitState, error) {
	return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, s.acquireErr
}

func (s *coordinatorTestStore) AdmitSchedule(context.Context, scheduler.AdmissionRequest) error {
	return errors.New("unexpected admission")
}

func (s *coordinatorTestStore) CompleteSchedule(context.Context, scheduler.CompletionRequest) (scheduler.CircuitState, error) {
	return scheduler.CircuitState{}, nil
}

func (s *coordinatorTestStore) QuarantineSchedule(context.Context, scheduler.QuarantineRequest) error {
	s.quarantineCalls++
	return nil
}

func (s *coordinatorTestStore) GetCircuitState(context.Context, string) (scheduler.CircuitState, error) {
	return scheduler.CircuitState{}, nil
}

func newWorkflowEnvironment(st store.DurableStore, exec executor.ReleaseExecutor) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := NewActivities(st, policy.New(), exec)
	env.RegisterActivity(activities.PersistRun)
	env.RegisterActivity(activities.EvaluateStep)
	env.RegisterActivity(activities.Deploy)
	env.RegisterActivity(activities.Verify)
	env.RegisterActivity(activities.Rollback)
	env.RegisterActivity(activities.AcquireSchedule)
	env.RegisterActivity(activities.CompleteSchedule)
	env.RegisterActivity(activities.RecoverExecution)
	return env
}

func workflowFixture(t *testing.T, approval bool, behaviors map[string]executor.MockBehavior) (domain.ReleaseRun, *store.Memory, *executor.Mock) {
	t.Helper()
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	environment := domain.EnvironmentStaging
	scopes := []string{"release:deploy"}
	if approval {
		environment = domain.EnvironmentProduction
		scopes = append(scopes, "release:production")
	}
	change := domain.Change{Service: "identity", DesiredVersion: "sha-1", CISuccess: true, DependenciesHealthy: true, DestructiveMigration: approval}
	run := domain.ReleaseRun{ID: "run-workflow", WorkflowID: "run-workflow", RequestID: "request-workflow", ReleaseVersion: "release-1", Environment: environment, RequestedBy: "requester", Agent: domain.AgentIdentity{ID: "agent", Scopes: scopes}, Plan: domain.ReleasePlan{Environment: environment, Hash: "plan-hash", PlanHash: "plan-hash", Phases: []domain.PlanPhase{{Number: 0, Steps: []domain.PlanStep{{Service: "identity", DesiredVersion: "sha-1", Phase: 0, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic}}}}}, Status: domain.RunPending, Steps: []domain.ReleaseStep{{Service: "identity", Phase: 0, Status: domain.StepPending, Change: change, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic}}, StateVersion: 1, CreatedAt: now, UpdatedAt: now}
	memory := store.NewMemory()
	if _, created, err := memory.CreateRun(&run); err != nil || !created {
		t.Fatalf("create fixture: %v", err)
	}
	return run, memory, executor.NewMock(behaviors)
}

type flakyUpdateStore struct {
	*store.Memory
	calls  int
	failAt int
}

func (s *flakyUpdateStore) UpdateRunAtomic(run *domain.ReleaseRun, expected int64, audit []domain.AuditEvent, outbox []domain.OutboxEvent) error {
	s.calls++
	if s.calls == s.failAt {
		return errors.New("simulated worker crash before transaction")
	}
	return s.Memory.UpdateRunAtomic(run, expected, audit, outbox)
}

type failCompleteStore struct {
	*store.Memory
	fail bool
}

func (s *failCompleteStore) CompleteExecution(record store.ExecutionRecord, expected int64, audit domain.AuditEvent, outbox domain.OutboxEvent) error {
	if s.fail {
		s.fail = false
		return errors.New("simulated commit response loss")
	}
	return s.Memory.CompleteExecution(record, expected, audit, outbox)
}

func TestProductionVerificationGuardBeforeAnyDispatch(t *testing.T) {
	for _, name := range []string{"run flag", "plan flag", "later step", "missing mapping", "duplicate mapping"} {
		t.Run(name, func(t *testing.T) {
			run, memory, exec := workflowFixture(t, true, nil)
			switch name {
			case "run flag":
				run.Steps[0].VerificationRequired = false
			case "plan flag":
				run.Plan.Phases[0].Steps[0].VerificationRequired = false
			case "later step":
				step := run.Steps[0]
				step.Service = "later"
				step.Change.Service = "later"
				step.VerificationRequired = false
				run.Steps = append(run.Steps, step)
				plan := run.Plan.Phases[0].Steps[0]
				plan.Service = "later"
				run.Plan.Phases[0].Steps = append(run.Plan.Phases[0].Steps, plan)
			case "missing mapping":
				run.Plan.Phases = nil
			case "duplicate mapping":
				run.Plan.Phases[0].Steps = append(run.Plan.Phases[0].Steps, run.Plan.Phases[0].Steps[0])
			}
			// Durable historical/tampered run, rather than only changed Workflow input.
			if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
				t.Fatal(err)
			}
			env := newWorkflowEnvironment(memory, exec)
			env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
			if err := env.GetWorkflowError(); err != nil {
				if name == "missing mapping" || name == "duplicate mapping" {
					if exec.DeployCalls(run.ID+"/production/identity/sha-1") != 0 {
						t.Fatal("malformed mapping dispatched")
					}
					return
				}
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != domain.RunBlocked || exec.DeployCalls(run.ID+"/production/identity/sha-1") != 0 {
				t.Fatalf("unsafe result: %#v", result)
			}
		})
	}
}

func TestQueuedProductionDeployAndUnsafeSuccessAreRejected(t *testing.T) {
	run, memory, exec := workflowFixture(t, true, nil)
	run.Steps[0].VerificationRequired = false
	if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	activities := NewActivities(memory, policy.New(), exec)
	input := DeployInput{RunID: run.ID, Environment: "production", Service: "identity", DesiredVersion: "sha-1", IdempotencyKey: "queued"}
	if _, err := activities.Deploy(context.Background(), input); err == nil {
		t.Fatal("queued unsafe deploy accepted")
	}
	if _, err := activities.Rollback(context.Background(), RollbackInput{Deploy: input}); err == nil {
		t.Fatal("unsafe queued rollback accepted")
	}
	for _, reservation := range []struct{ adapter, key string }{{"mock", "queued"}, {"mock.rollback", "queued/rollback"}} {
		if _, err := memory.GetExecution(reservation.adapter, reservation.key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unsafe activity reserved execution: %v", err)
		}
	}
	if exec.DeployCalls("queued") != 0 {
		t.Fatal("queued deploy reached executor")
	}
	run.Status = domain.RunSucceeded
	if _, err := activities.PersistRun(context.Background(), PersistInput{Run: run}); err == nil {
		t.Fatal("unsafe success persisted")
	}
	current, err := memory.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status == domain.RunSucceeded {
		t.Fatal("durable unsafe success")
	}
	run = *current
	run.Environment = domain.EnvironmentStaging
	run.Plan.Environment = domain.EnvironmentStaging
	if _, err := activities.PersistRun(context.Background(), PersistInput{Run: run}); err == nil {
		t.Fatal("environment downgrade persisted")
	}
}

func TestStagingExplicitVerificationSkipStillSucceeds(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	run.Steps[0].VerificationRequired = false
	run.Plan.Phases[0].Steps[0].VerificationRequired = false
	if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	env := newWorkflowEnvironment(memory, exec)
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunSucceeded {
		t.Fatalf("status=%s", result.Status)
	}
}

func TestEvaluateProductionVerificationChecksInputAndDurableFlags(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprintf("durable=%t", durable), func(t *testing.T) {
			run, memory, exec := workflowFixture(t, true, nil)
			unsafe := run
			unsafe.Steps = append([]domain.ReleaseStep(nil), run.Steps...)
			unsafe.Steps[0].VerificationRequired = false
			input := unsafe
			if durable {
				if err := memory.UpdateRunAtomic(&unsafe, unsafe.StateVersion, nil, nil); err != nil {
					t.Fatal(err)
				}
				input = run
			}
			decision, err := NewActivities(memory, policy.New(), exec).EvaluateStep(context.Background(), EvaluateInput{Run: input, Step: input.Steps[0]})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Decision != domain.DecisionDeny || decision.ReasonCode != "PRODUCTION_VERIFICATION_REQUIRED" {
				t.Fatalf("decision=%#v", decision)
			}
		})
	}
}

func TestPersistCannotReplaceProductionVerificationConfiguration(t *testing.T) {
	for _, unsafeDurable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsafeDurable=%t", unsafeDurable), func(t *testing.T) {
			run, memory, exec := workflowFixture(t, true, nil)
			if unsafeDurable {
				run.Steps[0].VerificationRequired = false
				if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			run.Steps[0].VerificationRequired = unsafeDurable
			if _, err := NewActivities(memory, policy.New(), exec).PersistRun(context.Background(), PersistInput{Run: run}); err == nil {
				t.Fatal("changed production verification flags persisted")
			}
			current, err := memory.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Steps[0].VerificationRequired == unsafeDurable {
				t.Fatal("durable verification flags changed")
			}
		})
	}
}

func TestPersistProductionPlanAcceptsSerializationRoundTrip(t *testing.T) {
	run, memory, exec := workflowFixture(t, true, nil)
	run.Plan.CreatedAt = time.Date(2026, 10, 16, 12, 0, 0, 123, time.FixedZone("JST", 9*60*60))
	run.Plan.ExpiresAt = run.Plan.CreatedAt.Add(time.Hour)
	run.Plan.ContractHashes = map[string]string{}
	run.Plan.Phases[0].Steps[0].Dependencies = []domain.Dependency{}
	if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip domain.ReleaseRun
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	roundTrip.Plan.CreatedAt = roundTrip.Plan.CreatedAt.UTC()
	roundTrip.Plan.ExpiresAt = roundTrip.Plan.ExpiresAt.UTC()
	roundTrip.Status = domain.RunRunning
	if _, err := NewActivities(memory, policy.New(), exec).PersistRun(context.Background(), PersistInput{Run: roundTrip}); err != nil {
		t.Fatalf("safe serialized plan rejected: %v", err)
	}
}
