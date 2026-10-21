package scenario

import (
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"

	"go.temporal.io/sdk/testsuite"
)

func TestDeploySuccessRequiresPassingEvidence(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, nil)
	environment := scenarioEnvironment(memory, releaseExecutor)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Status != domain.RunSucceeded {
		t.Fatalf("status=%s", result.Status)
	}
	for _, step := range result.Steps {
		if step.Status != domain.StepSucceeded || step.Verification == nil || step.Verification.Status != domain.VerificationPass || step.Verification.Evidence.EvidenceHash == "" {
			t.Fatalf("step=%#v", step)
		}
	}
}

func scenarioWorkflowFixture(t *testing.T, behavior map[string]executor.MockBehavior) (domain.ReleaseRun, *store.Memory, *executor.Mock) {
	t.Helper()
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	steps := []domain.ReleaseStep{
		{Service: "identity", Phase: 0, Status: domain.StepPending, Change: domain.Change{Service: "identity", DesiredVersion: "sha-identity", CISuccess: true, DependenciesHealthy: true}, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic},
		{Service: "payments", Phase: 1, Status: domain.StepPending, Change: domain.Change{Service: "payments", DesiredVersion: "sha-payments", CISuccess: true, DependenciesHealthy: true}, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic},
	}
	planSteps := []domain.PlanStep{
		{Service: "identity", DesiredVersion: "sha-identity", Phase: 0, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic},
		{Service: "payments", DesiredVersion: "sha-payments", Phase: 1, Dependencies: []domain.Dependency{{Service: "identity", Type: domain.DependencyRolloutOrder}}, VerificationRequired: true, ObservationWindow: "1s", RollbackMode: domain.RollbackAutomatic},
	}
	run := domain.ReleaseRun{ID: "scenario-run", WorkflowID: "scenario-run", RequestID: "scenario-request", ReleaseVersion: "release-1", Environment: domain.EnvironmentStaging, RequestedBy: "requester", Agent: domain.AgentIdentity{ID: "agent", Scopes: []string{"release:deploy"}}, Plan: domain.ReleasePlan{Hash: "plan-hash", PlanHash: "plan-hash", Phases: []domain.PlanPhase{{Number: 0, Steps: planSteps}}}, Status: domain.RunPending, Steps: steps, StateVersion: 1, CreatedAt: now, UpdatedAt: now}
	memory := store.NewMemory()
	if _, created, err := memory.CreateRun(&run); err != nil || !created {
		t.Fatalf("create run: created=%v err=%v", created, err)
	}
	return run, memory, executor.NewMock(behavior)
}

func scenarioEnvironment(memory *store.Memory, releaseExecutor executor.ReleaseExecutor) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	activities := workflowcore.NewActivities(memory, policy.New(), releaseExecutor)
	environment.RegisterActivity(activities.PersistRun)
	environment.RegisterActivity(activities.EvaluateStep)
	environment.RegisterActivity(activities.Deploy)
	environment.RegisterActivity(activities.Verify)
	environment.RegisterActivity(activities.Rollback)
	environment.RegisterActivity(activities.RecoverExecution)
	environment.RegisterActivity(activities.AcquireSchedule)
	environment.RegisterActivity(activities.CompleteSchedule)
	return environment
}

func scenarioResult(t *testing.T, environment *testsuite.TestWorkflowEnvironment) domain.ReleaseRun {
	t.Helper()
	if err := environment.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result domain.ReleaseRun
	if err := environment.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
