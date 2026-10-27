package application

import (
	"context"
	"encoding/base64"
	"go.temporal.io/sdk/testsuite"
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/store"
	wf "themisy/internal/workflow"
	"themisy/pkg/protocol"
	"time"
)

type observationTamperSigner struct{ grant.Signer }

func (s observationTamperSigner) Sign(ctx context.Context, p []byte) (protocol.Signature, error) {
	sig, err := s.Signer.Sign(ctx, p)
	if err == nil && strings.Contains(string(p), `"runtime":`) {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(sig.Value)
		if decodeErr != nil {
			return sig, decodeErr
		}
		raw[0] ^= 1
		sig.Value = base64.RawURLEncoding.EncodeToString(raw)
	}
	return sig, err
}

type observationWindowExecutor struct{ executor.ReleaseExecutor }

func (e observationWindowExecutor) Verify(ctx context.Context, d executor.Deployment) (executor.VerificationResult, error) {
	time.Sleep(2 * time.Millisecond)
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return e.ReleaseExecutor.Verify(bounded, d)
}

func TestWorkflowConsumesOnlyAuthenticatedObservationAndReverifiesRollback(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "bad-observation-signature"}[tamper], func(t *testing.T) {
			control := store.NewMemory()
			option := observationFlowOptions{FixtureHealth: true, Tamper: tamper}
			option.Prepare = func(run *domain.ReleaseRun) {
				run.Plan.Environment = run.Environment
				for i := range run.Plan.Phases[0].Steps {
					p := &run.Plan.Phases[0].Steps[i]
					p.VerificationRequired = true
					p.DesiredVersion = run.Steps[1].Change.DesiredVersion
					p.ObservationWindow = "1ms"
					p.Scheduling.RiskTier = "low"
					p.Scheduling.Environment = run.Environment
				}
				for i := range run.Steps {
					s := &run.Steps[i]
					s.VerificationRequired = true
					s.Change.Service = s.Service
					s.Change.DesiredVersion = run.Steps[1].Change.DesiredVersion
					s.Change.Risk = "low"
					s.Approval = nil
					s.ObservationWindow = "1ms"
					s.RollbackMode = domain.RollbackAutomatic
				}
				run.Steps[1].Status = domain.StepPending
			}
			option.Drive = func(run *domain.ReleaseRun, exec *GrantExecutor, source *observationFlowSource) {
				var suite testsuite.WorkflowTestSuite
				env := suite.NewTestWorkflowEnvironment()
				env.SetStartTime(time.Now().UTC())
				a := wf.NewActivities(control, exec.Grants.Policy, observationWindowExecutor{exec})
				env.RegisterActivity(a.PersistRun)
				env.RegisterActivity(a.EvaluateStep)
				env.RegisterActivity(a.AcquireSchedule)
				env.RegisterActivity(a.CompleteSchedule)
				env.RegisterActivity(a.Deploy)
				env.RegisterActivity(a.Verify)
				env.RegisterActivity(a.Rollback)
				env.RegisterActivity(a.RecoverExecution)
				env.ExecuteWorkflow(wf.ReleaseWorkflow, wf.ReleaseInput{Run: *run})
				if err := env.GetWorkflowError(); err != nil {
					t.Fatal(err)
				}
				var result domain.ReleaseRun
				if err := env.GetWorkflowResult(&result); err != nil {
					t.Fatal(err)
				}
				if tamper {
					if result.Status != domain.RunBlocked || result.Steps[1].Status != domain.StepBlocked || source.writes.Load() != 1 || result.Steps[1].Verification != nil {
						t.Fatalf("invalid signature advanced workflow: run=%s step=%s writes=%d", result.Status, result.Steps[1].Status, source.writes.Load())
					}
				} else {
					v := result.Steps[1].RollbackVerification
					if result.Status != domain.RunFailed || result.Steps[1].Status != domain.StepRolledBack || source.writes.Load() != 2 || v == nil || v.Runtime == nil || v.ObservationGrantID == "" || v.SourceGrantID == "" {
						t.Fatalf("rollback not reverified: run=%s step=%s writes=%d verification=%+v", result.Status, result.Steps[1].Status, source.writes.Load(), v)
					}
				}
			}
			testObservationFlow(t, control, store.NewMemory(), option)
		})
	}
}
