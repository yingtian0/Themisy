package workflow

import (
	"context"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/pkg/adapter"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// These tests isolate the Workflow's consumer of an already-verified recovery
// result. Grant signature/binding and persistence have separate Activity tests.
func TestRecoveredDeploymentStillRequiresVerification(t *testing.T) {
	for _, status := range []adapter.VerificationStatus{adapter.VerificationPass, adapter.VerificationFail, adapter.VerificationMissing, adapter.VerificationInconclusive, "UNAVAILABLE", "REJECTED"} {
		t.Run(string(status), func(t *testing.T) {
			run, memory, exec := workflowFixture(t, false, nil)
			run.Steps[0].RollbackMode = domain.RollbackManualOnly
			a := NewActivities(memory, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: "existing-grant"})
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivity(a.PersistRun)
			env.RegisterActivity(a.EvaluateStep)
			env.RegisterActivity(a.AcquireSchedule)
			env.RegisterActivity(a.CompleteSchedule)
			env.RegisterActivity(a.Deploy)
			recovered := executor.Deployment{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/staging/identity/sha-1", ExternalID: "recovered", StartedAt: run.CreatedAt, FinishedAt: run.CreatedAt}
			recoveryCalls, verifyCalls := 0, 0
			env.RegisterActivityWithOptions(func(context.Context, RecoverExecutionInput) (RecoverExecutionResult, error) {
				recoveryCalls++
				return RecoverExecutionResult{Supported: true, Ready: true, Failed: status == "REJECTED", Deployment: recovered, Execution: domain.Execution{ID: "existing-execution", ExternalExecutionID: recovered.ExternalID}}, nil
			}, activity.RegisterOptions{Name: ActivityRecoverExecution})
			env.RegisterActivityWithOptions(func(_ context.Context, input VerifyInput) (executor.VerificationResult, error) {
				verifyCalls++
				if input.Deployment != recovered {
					t.Errorf("verification did not receive exact recovered deployment: %+v", input.Deployment)
				}
				if status == "UNAVAILABLE" {
					return executor.VerificationResult{}, temporal.NewNonRetryableApplicationError("no evidence", "UNAVAILABLE", nil)
				}
				return executor.VerificationResult{Status: status}, nil
			}, activity.RegisterOptions{Name: ActivityVerify})
			env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			want := domain.RunEscalated
			if status == adapter.VerificationPass {
				want = domain.RunSucceeded
			}
			if status == "UNAVAILABLE" {
				want = domain.RunBlocked
			}
			wantVerify := 1
			if status == "REJECTED" {
				wantVerify = 0
			}
			if result.Status != want || verifyCalls != wantVerify || recoveryCalls != 1 {
				t.Fatalf("run=%s want=%s verify=%d recovery=%d", result.Status, want, verifyCalls, recoveryCalls)
			}
			if status == "REJECTED" && result.Steps[0].Status != domain.StepEscalated {
				t.Fatalf("rejected recovery left step %s", result.Steps[0].Status)
			}
			if exec.DeployCalls(recovered.IdempotencyKey) != 0 {
				t.Fatal("recovery issued provider write")
			}
		})
	}
}

func TestLegacyUnknownHistoryBranchDoesNotScheduleRecovery(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	a := NewActivities(memory, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: "existing-grant"})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.OnGetVersion("persistent-runner-recovery-wait", temporalworkflow.DefaultVersion, temporalworkflow.Version(1)).Return(temporalworkflow.DefaultVersion).Once()
	env.OnGetVersion("persistent-schedule-coordinator", temporalworkflow.DefaultVersion, temporalworkflow.Version(1)).Return(temporalworkflow.DefaultVersion).Once()
	env.RegisterActivity(a.PersistRun)
	env.RegisterActivity(a.EvaluateStep)
	env.RegisterActivity(a.AcquireSchedule)
	env.RegisterActivity(a.CompleteSchedule)
	env.RegisterActivity(a.Deploy)
	env.RegisterActivity(a.RecoverExecution)
	env.OnActivity(ActivityRecoverExecution, mock.Anything, mock.Anything).Never()
	env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.RunBlocked || result.Steps[0].Status != domain.StepUnknown {
		t.Fatalf("legacy branch advanced: %s/%s", result.Status, result.Steps[0].Status)
	}
	env.AssertExpectations(t)
}

func TestRecoveredRollbackRequiresIndependentVerification(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		status                   adapter.VerificationStatus
		rejected, cancel, legacy bool
		want                     domain.RunStatus
		wantStep                 domain.StepStatus
	}{
		{name: "pass", status: adapter.VerificationPass, want: domain.RunFailed, wantStep: domain.StepRolledBack},
		{name: "fail", status: adapter.VerificationFail, want: domain.RunEscalated, wantStep: domain.StepEscalated},
		{name: "missing", status: adapter.VerificationMissing, want: domain.RunEscalated, wantStep: domain.StepEscalated},
		{name: "inconclusive", status: adapter.VerificationInconclusive, want: domain.RunEscalated, wantStep: domain.StepEscalated},
		{name: "unavailable", want: domain.RunEscalated, wantStep: domain.StepEscalated},
		{name: "rejected", rejected: true, want: domain.RunEscalated, wantStep: domain.StepEscalated},
		{name: "cancel-pending", cancel: true, want: domain.RunCancelled, wantStep: domain.StepUnknown},
		{name: "legacy", legacy: true, want: domain.RunBlocked, wantStep: domain.StepUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, memory, exec := workflowFixture(t, false, nil)
			a := NewActivities(memory, policy.New(), exec)
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			if tc.legacy {
				env.OnGetVersion("persistent-runner-recovery-wait", temporalworkflow.DefaultVersion, temporalworkflow.Version(1)).Return(temporalworkflow.DefaultVersion).Once()
			}
			env.RegisterActivity(a.PersistRun)
			env.RegisterActivity(a.EvaluateStep)
			env.RegisterActivity(a.AcquireSchedule)
			env.RegisterActivity(a.CompleteSchedule)
			env.RegisterActivity(a.Deploy)
			key := run.ID + "/staging/identity/sha-1"
			recovered := executor.Deployment{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: key + "/rollback", ExternalID: "restored-rollback", StartedAt: run.CreatedAt, FinishedAt: run.CreatedAt}
			rollbackCalls, recoveryCalls, verifyCalls := 0, 0, 0
			env.RegisterActivityWithOptions(func(_ context.Context, input RollbackInput) (RollbackResult, error) {
				rollbackCalls++
				if input.Deploy.IdempotencyKey != key || input.Deployment.ExternalID == "" {
					t.Errorf("incorrect rollback source: %+v", input)
				}
				return RollbackResult{}, temporal.NewNonRetryableApplicationError("existing rollback Grant unresolved", "UNKNOWN_EXTERNAL_STATE", nil)
			}, activity.RegisterOptions{Name: ActivityRollback})
			env.RegisterActivityWithOptions(func(_ context.Context, input RecoverExecutionInput) (RecoverExecutionResult, error) {
				recoveryCalls++
				if !input.Rollback || input.RunID != run.ID || input.Service != "identity" || input.Environment != "staging" || input.DesiredVersion != "sha-1" || input.IdempotencyKey != key {
					t.Errorf("incorrect recovery binding: %+v", input)
				}
				if tc.legacy {
					t.Error("legacy rollback must not schedule recovery")
				}
				if tc.cancel || recoveryCalls == 1 {
					return RecoverExecutionResult{Supported: true}, nil
				}
				return RecoverExecutionResult{Supported: true, Ready: true, Failed: tc.rejected, Deployment: recovered, Execution: domain.Execution{ID: "rollback-execution", ExternalExecutionID: recovered.ExternalID}}, nil
			}, activity.RegisterOptions{Name: ActivityRecoverExecution})
			env.RegisterActivityWithOptions(func(_ context.Context, input VerifyInput) (executor.VerificationResult, error) {
				verifyCalls++
				if verifyCalls == 1 {
					return executor.VerificationResult{Status: adapter.VerificationFail}, nil
				}
				if input.Deployment != recovered {
					t.Errorf("rollback verification lost restored fields: %+v", input.Deployment)
				}
				if !tc.status.Valid() {
					return executor.VerificationResult{}, temporal.NewNonRetryableApplicationError("verification unavailable", "UNAVAILABLE", nil)
				}
				return executor.VerificationResult{Status: tc.status}, nil
			}, activity.RegisterOptions{Name: ActivityVerify})
			if tc.cancel {
				env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalCancel, ControlSignal{Actor: "operator"}) }, time.Minute)
			}
			env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.want || result.Steps[0].Status != tc.wantStep {
				t.Fatalf("run/step=%s/%s want=%s/%s", result.Status, result.Steps[0].Status, tc.want, tc.wantStep)
			}
			wantVerify, wantRecovery := 2, 2
			if tc.rejected {
				wantVerify = 1
			}
			if tc.cancel {
				wantVerify, wantRecovery = 1, 1
			}
			if tc.legacy {
				wantVerify, wantRecovery = 1, 0
			}
			if rollbackCalls != 1 || recoveryCalls != wantRecovery || verifyCalls != wantVerify || exec.DeployCalls(key) != 1 {
				t.Fatalf("deploy=%d rollback=%d recovery=%d verification=%d", exec.DeployCalls(key), rollbackCalls, recoveryCalls, verifyCalls)
			}
			persisted, err := memory.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != tc.want || persisted.Steps[0].Status != tc.wantStep {
				t.Fatalf("durable outcome differs: %+v", persisted)
			}
			env.AssertExpectations(t)
		})
	}
}
