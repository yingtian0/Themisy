package scenario

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/scheduler"
	workflowcore "themisy/internal/workflow"

	"go.yaml.in/yaml/v3"
)

type scaleFixture struct {
	ServiceCount       int              `yaml:"service_count"`
	ServicesPerPhase   int              `yaml:"services_per_phase"`
	TeamCount          int              `yaml:"team_count"`
	ClusterCount       int              `yaml:"cluster_count"`
	FailureDomainCount int              `yaml:"failure_domain_count"`
	Limits             scheduler.Limits `yaml:"limits"`
	RunnerCapacity     int              `yaml:"runner_capacity"`
	AdapterRemaining   int              `yaml:"adapter_remaining"`
	QueueLimit         int              `yaml:"queue_limit"`
}

func TestTwoHundredServicesTenRunsHaveNoSafetyViolation(t *testing.T) {
	fixture := readScaleFixture(t)
	steps := generatedSteps(fixture)
	for iteration := range 10 {
		waves, err := scheduler.BuildWaves(steps, fixture.Limits)
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		assertWaveSafety(t, fixture, waves)
		dispatched := 0
		latencies := make([]time.Duration, 0, fixture.ServiceCount)
		for _, wave := range waves {
			coordinator, err := scheduler.New(fixture.Limits, scheduler.CircuitPolicy{MinimumSamples: 5, WindowSize: 10, ErrorRate: .5})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			results := coordinator.Dispatch(wave.Steps, scheduler.Capacity{RunnerAvailable: fixture.RunnerCapacity, AdapterRemaining: fixture.AdapterRemaining, QueueLimit: fixture.QueueLimit})
			for _, result := range results {
				if result.Err != nil || len(result.BlockedBy) > 0 {
					t.Fatalf("iteration %d wave %d blocked safe step: %#v", iteration, wave.Number, result)
				}
				latencies = append(latencies, time.Since(started))
				coordinator.Complete(result, false)
				dispatched++
			}
		}
		if dispatched != fixture.ServiceCount {
			t.Fatalf("iteration %d dispatched=%d want=%d", iteration, dispatched, fixture.ServiceCount)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p95 := latencies[(len(latencies)*95+99)/100-1]
		if p95 >= 5*time.Second {
			t.Fatalf("iteration %d eligible-to-dispatch p95=%s want <5s", iteration, p95)
		}
	}
}

func TestTemporalTwentyFourHourApprovalExpiryUsesTimeSkipping(t *testing.T) {
	run, memory, releaseExecutor := scenarioWorkflowFixture(t, nil)
	run.Environment = domain.EnvironmentProduction
	run.Plan.Environment = domain.EnvironmentProduction
	// The shared staging fixture flattens both steps into a single phase;
	// Production requires each step to match its enclosing Plan phase.
	plannedSteps := run.Plan.Phases[0].Steps
	run.Plan.Phases = []domain.PlanPhase{{Number: 0, Steps: plannedSteps[:1]}, {Number: 1, Steps: plannedSteps[1:]}}
	for i := range run.Plan.Phases {
		for j := range run.Plan.Phases[i].Steps {
			run.Plan.Phases[i].Steps[j].Scheduling.Environment = domain.EnvironmentProduction
		}
	}
	run.Agent.Scopes = append(run.Agent.Scopes, "release:production")
	run.Steps[0].Change.DestructiveMigration = true
	if err := memory.UpdateRun(&run, 1); err != nil {
		t.Fatal(err)
	}
	environment := scenarioEnvironment(memory, releaseExecutor)
	wallStarted := time.Now()
	environment.ExecuteWorkflow(workflowcore.ReleaseWorkflow, workflowcore.ReleaseInput{Run: run})
	result := scenarioResult(t, environment)
	if result.Status != domain.RunBlocked || result.Steps[0].Approval == nil || result.Steps[0].Approval.Status != domain.ApprovalExpired {
		t.Fatalf("result=%#v", result)
	}
	if time.Since(wallStarted) >= time.Minute {
		t.Fatal("Temporal test did not time-skip the 24-hour approval timer")
	}
}

func readScaleFixture(t *testing.T) scaleFixture {
	t.Helper()
	data, err := os.ReadFile("../fixtures/scenarios/200-services.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture scaleFixture
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.ServiceCount != 200 || fixture.ServicesPerPhase < 1 {
		t.Fatalf("invalid fixture: %#v", fixture)
	}
	return fixture
}

func generatedSteps(fixture scaleFixture) []scheduler.Step {
	steps := make([]scheduler.Step, fixture.ServiceCount)
	for index := range fixture.ServiceCount {
		dependencies := []string(nil)
		if index >= fixture.ServicesPerPhase {
			dependencies = []string{fmt.Sprintf("service-%03d", index-fixture.ServicesPerPhase)}
		}
		steps[index] = scheduler.Step{ID: fmt.Sprintf("service-%03d", index), Phase: index / fixture.ServicesPerPhase, Tenant: "tenant-a", Environment: "production", Region: "ap-northeast-1", Cluster: fmt.Sprintf("cluster-%d", index%fixture.ClusterCount), Team: fmt.Sprintf("team-%d", index%fixture.TeamCount), RiskTier: []string{"low", "medium", "high", "critical"}[index%4], FailureDomains: []string{fmt.Sprintf("domain-%02d", index%fixture.FailureDomainCount)}, Dependencies: dependencies}
	}
	return steps
}

func assertWaveSafety(t *testing.T, fixture scaleFixture, waves []scheduler.Wave) {
	t.Helper()
	waveByService := make(map[string]int, fixture.ServiceCount)
	for _, wave := range waves {
		if len(wave.Steps) > fixture.Limits.TenantGlobal {
			t.Fatalf("wave %d size=%d exceeds tenant limit=%d", wave.Number, len(wave.Steps), fixture.Limits.TenantGlobal)
		}
		failureDomains := map[string]bool{}
		for _, step := range wave.Steps {
			for _, dependency := range step.Dependencies {
				if dependencyWave, exists := waveByService[dependency]; !exists || dependencyWave >= wave.Number {
					t.Fatalf("dependency order violation: %s depends on %s", step.ID, dependency)
				}
			}
			for _, failureDomain := range step.FailureDomains {
				if failureDomains[failureDomain] {
					t.Fatalf("shared failure domain %s in wave %d", failureDomain, wave.Number)
				}
				failureDomains[failureDomain] = true
			}
			waveByService[step.ID] = wave.Number
		}
	}
}
