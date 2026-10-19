package workflow

import (
	"context"
	"testing"

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
	for _, status := range []adapter.VerificationStatus{adapter.VerificationPass, adapter.VerificationFail, adapter.VerificationMissing, adapter.VerificationInconclusive, "UNAVAILABLE"} {
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
				return RecoverExecutionResult{Supported: true, Ready: true, Deployment: recovered, Execution: domain.Execution{ID: "existing-execution", ExternalExecutionID: recovered.ExternalID}}, nil
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
			if result.Status != want || verifyCalls != 1 || recoveryCalls != 1 {
				t.Fatalf("run=%s want=%s verify=%d recovery=%d", result.Status, want, verifyCalls, recoveryCalls)
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
