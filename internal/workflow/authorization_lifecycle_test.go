package workflow

import (
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"time"
)

func TestAuthorizationLifetimeStopsWithoutExtendingPlan(t *testing.T) {
	for _, scenario := range []string{"late-approval", "later-step", "last-observation", "multi-observation", "failed-observation", "failed-boundary"} {
		t.Run(scenario, func(t *testing.T) {
			approval := scenario == "late-approval"
			behaviors := map[string]executor.MockBehavior{}
			if scenario == "failed-observation" || scenario == "failed-boundary" {
				behaviors["identity"] = executor.MockBehavior{VerifyHealthy: false}
			}
			run, memory, exec := workflowFixture(t, approval, behaviors)
			start := time.Now().UTC()
			run.Plan.CreatedAt, run.Plan.ExpiresAt = start, start.Add(15*time.Minute)
			run.Steps[0].ObservationWindow = "16m"
			run.Plan.Phases[0].Steps[0].ObservationWindow = "16m"
			if scenario == "failed-boundary" {
				run.Steps[0].ObservationWindow = "15m"
				run.Plan.Phases[0].Steps[0].ObservationWindow = "15m"
			}
			if scenario == "later-step" || scenario == "multi-observation" {
				next := run.Steps[0]
				next.Service = "payment"
				next.Change.Service = "payment"
				run.Steps = append(run.Steps, next)
				planned := run.Plan.Phases[0].Steps[0]
				planned.Service = "payment"
				run.Plan.Phases[0].Steps = append(run.Plan.Phases[0].Steps, planned)
			}
			if scenario == "multi-observation" {
				for i := range run.Steps {
					run.Steps[i].ObservationWindow = "8m"
					run.Plan.Phases[0].Steps[i].ObservationWindow = "8m"
				}
			}
			if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
				t.Fatal(err)
			}
			env := newWorkflowEnvironment(memory, exec)
			env.SetStartTime(start)
			if approval {
				env.RegisterDelayedCallback(func() {
					env.SignalWorkflow(SignalApproval, ApprovalSignal{ApprovalID: deterministicID("approval", run.ID, "identity"), Actor: "sre-user", Roles: []string{"sre", "service-owner"}, Approve: true})
				}, 16*time.Minute)
			}
			env.ExecuteWorkflow(ReleaseWorkflow, ReleaseInput{Run: run})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if !result.Plan.ExpiresAt.Equal(run.Plan.ExpiresAt) {
				t.Fatal("old Plan lifetime extended")
			}
			if scenario == "last-observation" || scenario == "multi-observation" {
				if result.Status != domain.RunSucceeded || result.Steps[0].Verification == nil {
					t.Fatalf("late read-only result was dropped: %+v", result)
				}
				return
			}
			if result.Status != domain.RunBlocked || result.AuthorizationStop == nil || result.AuthorizationStop.ReasonCode != "REAUTHORIZATION_REQUIRED" {
				t.Fatalf("missing explicit stop: %+v", result)
			}
			service := "identity"
			if scenario == "later-step" {
				service = "payment"
			}
			if scenario == "late-approval" || scenario == "later-step" {
				if calls := exec.DeployCalls(run.ID + "/" + string(run.Environment) + "/" + service + "/sha-1"); calls != 0 {
					t.Fatalf("expired write calls=%d", calls)
				}
			}
			if (scenario == "failed-observation" || scenario == "failed-boundary") && result.Steps[0].RollbackExecution != nil {
				t.Fatal("expired authority produced rollback")
			}
			found := false
			for _, step := range result.Steps {
				if strings.Contains(step.Failure, "REAUTHORIZATION_REQUIRED") {
					found = true
				}
			}
			if !found {
				t.Fatal("missing user-facing reason")
			}
		})
	}
}

func TestCompensationBoundaryRequiresKnownSourceAndTarget(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	run, _, _ := workflowFixture(t, false, nil)
	run.Plan.CreatedAt = now
	run.Plan.ExpiresAt = now.Add(15 * time.Minute)
	planned := &run.Plan.Phases[0].Steps[0]
	planned.RollbackDeadline = "2m"
	planned.RollbackTargetHash = "sha256:" + strings.Repeat("a", 64)
	source := executor.Deployment{GrantID: "source", ExternalID: "external", StartedAt: now}
	run.Steps[0].Execution = &domain.Execution{Status: "SUCCEEDED", ExternalExecutionID: "external"}
	if stop := compensationRefusal(run, run.Steps[0].Service, source, now.Add(2*time.Minute-time.Nanosecond)); stop != nil {
		t.Fatalf("valid permission: %+v", stop)
	}
	if stop := compensationRefusal(run, run.Steps[0].Service, source, now.Add(2*time.Minute)); stop == nil || stop.ReasonCode != "COMPENSATION_EXPIRED" {
		t.Fatalf("deadline: %+v", stop)
	}
	planned.RollbackTargetHash = ""
	if stop := compensationRefusal(run, run.Steps[0].Service, source, now); stop == nil || stop.ReasonCode != "COMPENSATION_UNAVAILABLE" {
		t.Fatalf("missing predecessor: %+v", stop)
	}
}
