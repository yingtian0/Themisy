package scenario

import (
	"context"
	"strings"
	"testing"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/executor"
	workflowcore "themisy/internal/workflow"
)

// Only deployment is simulated. Verification uses the same unavailable
// GrantExecutor behavior as the running binary, never the mock's default PASS.
type successfulDispatchWithoutVerifier struct {
	*executor.Mock
	verification *application.GrantExecutor
}

func (e successfulDispatchWithoutVerifier) Verify(ctx context.Context, deployment executor.Deployment) (executor.VerificationResult, error) {
	return e.verification.Verify(ctx, deployment)
}

func TestSuccessfulDispatchCannotSucceedWithoutRuntimeVerification(t *testing.T) {
	run, memory, dispatch := scenarioWorkflowFixture(t, nil)
	runtime := successfulDispatchWithoutVerifier{Mock: dispatch, verification: &application.GrantExecutor{}}
	environment := scenarioEnvironment(memory, runtime)
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Steps[0].Execution == nil || result.Steps[0].Execution.ExternalExecutionID == "" || dispatch.DeployCalls("scenario-run/staging/identity/sha-identity") != 1 {
		t.Fatalf("fixture did not complete dispatch: %#v", result.Steps[0].Execution)
	}
	if result.Status != domain.RunBlocked || result.Steps[0].Status != domain.StepBlocked || !strings.Contains(result.Steps[0].Failure, "verification unavailable") || result.Steps[0].Verification != nil {
		t.Fatalf("successful dispatch bypassed unavailable verification: %#v", result)
	}
	if result.Steps[1].Status != domain.StepCancelled || dispatch.DeployCalls("scenario-run/staging/payments/sha-payments") != 0 {
		t.Fatal("downstream deployment continued without runtime verification")
	}
	persisted, err := memory.GetRun(run.ID)
	if err != nil || persisted.Status != domain.RunBlocked || persisted.Steps[1].Status != domain.StepCancelled {
		t.Fatalf("blocked outcome did not persist: %#v err=%v", persisted, err)
	}
}
