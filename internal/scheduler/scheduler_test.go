package scheduler

import (
	"errors"
	"fmt"
	"testing"
)

func TestBudgetAppliesEveryDimensionAndServiceExclusivity(t *testing.T) {
	limits := DefaultLimits()
	limits.TenantGlobal = 2
	limits.Team = map[string]int{"payments": 1}
	budget, err := NewBudget(limits)
	if err != nil {
		t.Fatal(err)
	}
	first := testStep("one")
	first.Team = "payments"
	reservation, blocked := budget.TryAcquire(first)
	if len(blocked) != 0 {
		t.Fatalf("first blocked: %#v", blocked)
	}
	second := testStep("two")
	second.Team = "payments"
	if _, blocked = budget.TryAcquire(second); len(blocked) != 1 || blocked[0].Dimension != DimensionTeam {
		t.Fatalf("team intersection not enforced: %#v", blocked)
	}
	duplicate := first
	if _, blocked = budget.TryAcquire(duplicate); !hasDimension(blocked, DimensionService) {
		t.Fatalf("service/environment exclusivity not enforced: %#v", blocked)
	}
	budget.Release(reservation)
	if _, blocked = budget.TryAcquire(second); len(blocked) != 0 {
		t.Fatalf("released capacity remains blocked: %#v", blocked)
	}
}

func TestConstraintsForReturnsEveryDimensionOnceInStableOrder(t *testing.T) {
	limits := DefaultLimits()
	limits.Environment = map[string]int{"production": 3}
	step := testStep("payments")
	step.Environment = "production"
	step.FailureDomains = []string{"primary-db", "primary-db", "regional-api"}
	constraints, err := ConstraintsFor(step, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(constraints) != 9 {
		t.Fatalf("constraints=%#v", constraints)
	}
	for index := 1; index < len(constraints); index++ {
		previous, current := constraints[index-1], constraints[index]
		if previous.Dimension > current.Dimension || previous.Dimension == current.Dimension && previous.Key >= current.Key {
			t.Fatalf("constraints are not strictly ordered: %#v", constraints)
		}
	}
	for _, constraint := range constraints {
		if constraint.Dimension == DimensionEnvironment && constraint.Key == "tenant-a/production" && constraint.Limit != 3 {
			t.Fatalf("environment limit=%d", constraint.Limit)
		}
	}
}

func TestSharedFailureDomainNeverSharesAWave(t *testing.T) {
	limits := DefaultLimits()
	steps := []Step{testStep("a"), testStep("b"), testStep("c")}
	steps[0].FailureDomains = []string{"primary-db"}
	steps[1].FailureDomains = []string{"primary-db"}
	waves, err := BuildWaves(steps, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 2 || len(waves[0].Steps) != 2 || len(waves[1].Steps) != 1 {
		t.Fatalf("waves=%#v", waves)
	}
	for _, wave := range waves {
		seen := map[string]bool{}
		for _, step := range wave.Steps {
			for _, failureDomain := range step.FailureDomains {
				if seen[failureDomain] {
					t.Fatalf("failure domain %q shared in wave %#v", failureDomain, wave)
				}
				seen[failureDomain] = true
			}
		}
	}
}

func TestZeroRunnerCapacityAndOpenCircuitDispatchNothing(t *testing.T) {
	s, err := New(DefaultLimits(), CircuitPolicy{MinimumSamples: 2, WindowSize: 4, ErrorRate: .5})
	if err != nil {
		t.Fatal(err)
	}
	step := testStep("production")
	step.Environment = "production"
	result := s.Dispatch([]Step{step}, Capacity{RunnerAvailable: 0, AdapterRemaining: 10, QueueLimit: 10})
	if len(result) != 1 || !errors.Is(result[0].Err, ErrBackpressure) || result[0].Pressure != BackpressureRunnerCapacity {
		t.Fatalf("zero-capacity result=%#v", result)
	}
	s.breaker.Record(step.Tenant, step.Environment, true)
	s.breaker.Record(step.Tenant, step.Environment, false)
	result = s.Dispatch([]Step{step}, Capacity{RunnerAvailable: 10, AdapterRemaining: 10, QueueLimit: 10})
	if len(result) != 1 || !errors.Is(result[0].Err, ErrCircuitOpen) || len(result[0].Reservation.keys) != 0 {
		t.Fatalf("open-circuit result=%#v", result)
	}
}

func TestWavesRespectDependenciesAndLimits(t *testing.T) {
	limits := DefaultLimits()
	limits.TenantGlobal = 2
	steps := []Step{testStep("root-a"), testStep("root-b"), testStep("child")}
	steps[2].Phase = 1
	steps[2].Dependencies = []string{"root-a", "root-b"}
	waves, err := BuildWaves(steps, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 2 || len(waves[0].Steps) != 2 || waves[1].Steps[0].ID != "child" {
		t.Fatalf("waves=%#v", waves)
	}
}

func testStep(id string) Step {
	return Step{ID: id, Tenant: "tenant-a", Environment: "staging", Region: "ap-northeast-1", Cluster: "cluster-a", Team: id, RiskTier: "medium"}
}

func hasDimension(blocked []Blocked, dimension Dimension) bool {
	for _, reason := range blocked {
		if reason.Dimension == dimension {
			return true
		}
	}
	return false
}

func BenchmarkBuildTwoHundredServiceWaves(b *testing.B) {
	steps := make([]Step, 200)
	for index := range steps {
		steps[index] = testStep(fmt.Sprintf("service-%03d", index))
		steps[index].Phase = index / 20
		steps[index].FailureDomains = []string{fmt.Sprintf("domain-%02d", index%25)}
		if index >= 20 {
			steps[index].Dependencies = []string{fmt.Sprintf("service-%03d", index-20)}
		}
	}
	limits := DefaultLimits()
	limits.TenantGlobal = 12
	b.ResetTimer()
	for range b.N {
		if _, err := BuildWaves(steps, limits); err != nil {
			b.Fatal(err)
		}
	}
}
