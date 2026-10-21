package scenario

import (
	"testing"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
	"themisy/pkg/adapter"
)

func TestRollbackAPIFailureRemainsUnknownWithDownstreamStopped(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, map[string]executor.MockBehavior{"identity": {VerifyStatus: adapter.VerificationFail, RollbackError: true}})
	environment := scenarioEnvironment(memory, releaseExecutor)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	// A provider error is not a verified rejection. The mock has no durable
	// Grant result to reconcile, so preserve UNKNOWN rather than invent failure.
	if result.Status != domain.RunBlocked || result.Steps[0].Status != domain.StepUnknown || result.Steps[1].Status != domain.StepCancelled {
		t.Fatalf("run=%s first=%s downstream=%s", result.Status, result.Steps[0].Status, result.Steps[1].Status)
	}
	key := run.ID + "/staging/identity/sha-identity"
	rollback, err := memory.GetExecution("mock.rollback", key+"/rollback")
	if err != nil || rollback.Status != store.ExecutionUnknown {
		t.Fatalf("rollback status=%s err=%v", rollback.Status, err)
	}
	if releaseExecutor.DeployCalls(key) != 1 || releaseExecutor.DeployCalls(run.ID+"/staging/payments/sha-payments") != 0 {
		t.Fatal("recovery repeated deploy or started downstream")
	}
	persisted, err := memory.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != domain.RunBlocked || persisted.Steps[0].Status != domain.StepUnknown || persisted.Steps[1].Status != domain.StepCancelled {
		t.Fatalf("durable outcome differs: %+v", persisted)
	}
}

func TestRollbackVerificationFailureEscalates(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, map[string]executor.MockBehavior{"identity": {VerifyStatus: adapter.VerificationFail, RollbackVerifyStatus: adapter.VerificationFail}})
	environment := scenarioEnvironment(memory, releaseExecutor)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Status != domain.RunEscalated || result.Steps[0].RollbackVerification == nil || result.Steps[0].RollbackVerification.Status != domain.VerificationFail {
		t.Fatalf("result=%#v", result)
	}
}
