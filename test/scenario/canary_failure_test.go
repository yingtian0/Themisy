package scenario

import (
	"testing"

	"themisy/internal/domain"
	"themisy/internal/executor"
	workflowcore "themisy/internal/workflow"
	"themisy/pkg/adapter"
)

func TestVerificationFourOutcomesRemainDistinct(t *testing.T) {
	tests := []struct {
		name   string
		status adapter.VerificationStatus
	}{
		{name: "pass", status: adapter.VerificationPass},
		{name: "fail", status: adapter.VerificationFail},
		{name: "inconclusive", status: adapter.VerificationInconclusive},
		{name: "missing", status: adapter.VerificationMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run, memory, releaseExecutor := scenarioWorkflowFixture(t, map[string]executor.MockBehavior{"identity": {VerifyStatus: test.status}})
			environment := scenarioEnvironment(memory, releaseExecutor)
			environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
			result := scenarioResult(t, environment)
			if result.Steps[0].Verification == nil || result.Steps[0].Verification.Status != domain.VerificationStatus(test.status) {
				t.Fatalf("verification status=%v, want %v", result.Steps[0].Verification, test.status)
			}
			if test.status == adapter.VerificationPass {
				if result.Status != domain.RunSucceeded {
					t.Fatalf("PASS result status=%s", result.Status)
				}
				return
			}
			if result.Status == domain.RunSucceeded || result.Steps[1].Status != domain.StepCancelled {
				t.Fatalf("non-PASS outcome was accepted or downstream ran: status=%s downstream=%s", result.Status, result.Steps[1].Status)
			}
		})
	}
}

func TestCanaryFailureStopsDownstreamAndVerifiesAutomaticRollback(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, map[string]executor.MockBehavior{"identity": {VerifyStatus: adapter.VerificationFail}})
	environment := scenarioEnvironment(memory, releaseExecutor)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Status != domain.RunFailed || result.Steps[0].Status != domain.StepRolledBack || result.Steps[1].Status != domain.StepCancelled {
		t.Fatalf("run=%s first=%s downstream=%s", result.Status, result.Steps[0].Status, result.Steps[1].Status)
	}
	if result.Steps[0].RollbackExecution == nil || result.Steps[0].RollbackVerification == nil || result.Steps[0].RollbackVerification.Status != domain.VerificationPass {
		t.Fatalf("rollback state=%#v", result.Steps[0])
	}
	if releaseExecutor.DeployCalls("scenario-run/staging/payments/sha-payments") != 0 {
		t.Fatal("downstream adapter was called after canary failure")
	}
}

func TestMissingEvidenceNeverPasses(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, map[string]executor.MockBehavior{"identity": {VerifyStatus: adapter.VerificationMissing}})
	environment := scenarioEnvironment(memory, releaseExecutor)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Status == domain.RunSucceeded || result.Steps[0].Verification == nil || result.Steps[0].Verification.Status != domain.VerificationMissing || result.Steps[1].Status != domain.StepCancelled {
		t.Fatalf("result=%#v", result)
	}
}
