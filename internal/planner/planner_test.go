package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/profile"
)

func TestPlanOrdersDependenciesAndIsDeterministic(t *testing.T) {
	p, err := New([]domain.Service{
		{Name: "operations", ReleasePhase: 0, Dependencies: []string{"payments"}},
		{Name: "identity", ReleasePhase: 0},
		{Name: "payments", ReleasePhase: 0, Dependencies: []string{"identity"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.ReleaseRequest{
		ReleaseVersion: "v1", Environment: domain.EnvironmentProduction,
		Changes: []domain.Change{
			{Service: "operations", DesiredVersion: "c"},
			{Service: "identity", DesiredVersion: "a"},
			{Service: "payments", DesiredVersion: "b"},
		},
	}
	plan, err := p.Plan(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Phases) != 3 {
		t.Fatalf("got %d phases, want 3", len(plan.Phases))
	}
	for index, want := range []string{"identity", "payments", "operations"} {
		if got := plan.Phases[index].Steps[0].Service; got != want {
			t.Fatalf("phase %d service = %q, want %q", index, got, want)
		}
	}
	if plan.PlanHash == "" || plan.PlanHash != plan.Hash || plan.ID == "" || plan.ContextSnapshotRef == "" || plan.ExpiresAt.IsZero() {
		t.Fatalf("incomplete plan v2 metadata: %#v", plan)
	}

	request.Changes[0], request.Changes[2] = request.Changes[2], request.Changes[0]
	second, err := p.Plan(request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Hash != second.Hash {
		t.Fatalf("plan hash changed with input order or generated time: %s != %s", plan.Hash, second.Hash)
	}
	request.Changes[0].Risk = "high"
	changed, err := p.Plan(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Hash == second.Hash {
		t.Fatal("plan hash did not bind change risk facts")
	}
}

func TestPlanIntentRejectsUnsupportedSchemaVersion(t *testing.T) {
	p, err := New([]domain.Service{{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	intent := IntentFromLegacy(domain.ReleaseRequest{
		Environment: domain.EnvironmentStaging,
		Changes:     []domain.Change{{Service: "identity", DesiredVersion: "sha"}},
	})
	intent.APIVersion = "execution.themisy.io/v2"
	_, err = p.PlanIntent(intent)
	if code, ok := domain.ReasonOf(err); !ok || code != domain.ReasonUnsupportedSchemaVersion {
		t.Fatalf("got %v, want %s", err, domain.ReasonUnsupportedSchemaVersion)
	}
}

func TestPlanUsesTypedDependencySemantics(t *testing.T) {
	contracts, profiles := loadExamples(t)
	p, err := NewFromContracts(contracts, profiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(domain.ReleaseRequest{
		ReleaseVersion: "release-1",
		Environment:    domain.EnvironmentProduction,
		Changes: []domain.Change{
			{Service: "operations-api", DesiredVersion: "operations"},
			{Service: "identity-api", DesiredVersion: "identity"},
			{Service: "payment-api", DesiredVersion: "payment"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Phases) != 3 {
		t.Fatalf("phases=%d, want 3: %#v", len(plan.Phases), plan.Phases)
	}
	for index, want := range []string{"identity-api", "payment-api", "operations-api"} {
		step := plan.Phases[index].Steps[0]
		if step.Service != want {
			t.Fatalf("phase %d service=%s, want %s", index, step.Service, want)
		}
		if step.ContractHash == "" || step.ProfileHash == "" || step.Profile == "" {
			t.Fatalf("step lacks pinned contract/profile: %#v", step)
		}
	}
	operations := plan.Phases[2].Steps[0]
	if len(operations.Dependencies) != 2 {
		t.Fatalf("operations dependencies=%v, want traffic and runtime", operations.Dependencies)
	}
	if operations.Dependencies[0].Type != domain.DependencyRuntime || operations.Dependencies[1].Type != domain.DependencyTraffic {
		t.Fatalf("typed dependencies were not retained: %v", operations.Dependencies)
	}

	for left, right := 0, len(contracts)-1; left < right; left, right = left+1, right-1 {
		contracts[left], contracts[right] = contracts[right], contracts[left]
	}
	for left, right := 0, len(profiles)-1; left < right; left, right = left+1, right-1 {
		profiles[left], profiles[right] = profiles[right], profiles[left]
	}
	reordered, err := NewFromContracts(contracts, profiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reordered.PlanIntent(IntentFromLegacy(domain.ReleaseRequest{
		ReleaseVersion: "release-1",
		Environment:    domain.EnvironmentProduction,
		Changes: []domain.Change{
			{Service: "payment-api", DesiredVersion: "payment"},
			{Service: "operations-api", DesiredVersion: "operations"},
			{Service: "identity-api", DesiredVersion: "identity"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Hash != plan.Hash {
		t.Fatalf("plan hash changed with contract/profile/intent input order: %s != %s", second.Hash, plan.Hash)
	}
}

func TestPlanDetectsSelectedDependencyCycle(t *testing.T) {
	p, err := New([]domain.Service{
		{Name: "a", Dependencies: []string{"b"}},
		{Name: "b", Dependencies: []string{"a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Plan(domain.ReleaseRequest{
		Environment: domain.EnvironmentStaging,
		Changes:     []domain.Change{{Service: "a", DesiredVersion: "1"}, {Service: "b", DesiredVersion: "1"}},
	})
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("got %v, want ErrDependencyCycle", err)
	}
	if code, ok := domain.ReasonOf(err); !ok || code != domain.ReasonDependencyCycle {
		t.Fatalf("got reason %q, want %q", code, domain.ReasonDependencyCycle)
	}
}

func TestPlanRejectsInvalidContractGraphAndCapabilities(t *testing.T) {
	profiles, err := profile.LoadDir("../../examples/profiles")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		load func(t *testing.T) []contract.ServiceContract
		code domain.ReasonCode
	}{
		{
			name: "unknown dependency",
			load: func(t *testing.T) []contract.ServiceContract {
				return []contract.ServiceContract{loadContract(t, "../../test/fixtures/contracts/invalid/unknown-dependency.yaml")}
			},
			code: domain.ReasonUnknownDependency,
		},
		{
			name: "unknown profile",
			load: func(t *testing.T) []contract.ServiceContract {
				return []contract.ServiceContract{loadContract(t, "../../test/fixtures/contracts/invalid/unknown-profile.yaml")}
			},
			code: domain.ReasonUnknownProfile,
		},
		{
			name: "missing capability",
			load: func(t *testing.T) []contract.ServiceContract {
				return []contract.ServiceContract{loadContract(t, "../../test/fixtures/contracts/invalid/missing-capability.yaml")}
			},
			code: domain.ReasonMissingCapability,
		},
		{
			name: "cycle",
			load: func(t *testing.T) []contract.ServiceContract {
				contracts, loadErr := contract.LoadDir("../../test/fixtures/contracts/invalid/cycle")
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				return contracts
			},
			code: domain.ReasonDependencyCycle,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewFromContracts(test.load(t), profiles, Options{})
			if err == nil {
				t.Fatalf("expected %s", test.code)
			}
			if code, ok := domain.ReasonOf(err); !ok || code != test.code {
				t.Fatalf("got %v, want reason %s", err, test.code)
			}
		})
	}
}

func TestPlanInvalidatesOnContractAndProfileChanges(t *testing.T) {
	contracts, profiles := loadExamples(t)
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	p, err := NewFromContracts(contracts, profiles, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(exampleRequest())
	if err != nil {
		t.Fatal(err)
	}

	changedContracts := append([]contract.ServiceContract(nil), contracts...)
	changedContracts[0].Metadata.Owner += "-changed"
	changedContracts[0].ContentHash = ""
	contractPlanner, err := NewFromContracts(changedContracts, profiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := contractPlanner.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonContractChanged {
		t.Fatalf("contract invalidation error=%v", err)
	}

	changedProfiles := append([]profile.ReleaseProfile(nil), profiles...)
	changedProfiles[0].Spec.Deployment.Strategy += "-changed"
	changedProfiles[0].ContentHash = ""
	profilePlanner, err := NewFromContracts(contracts, changedProfiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := profilePlanner.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonProfileChanged {
		t.Fatalf("profile invalidation error=%v", err)
	}

	contextPlanner, err := NewFromContracts(contracts, profiles, Options{ContextSnapshotRef: "snapshot:changed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := contextPlanner.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonContextChanged {
		t.Fatalf("context invalidation error=%v", err)
	}

	policyPlanner, err := NewFromContracts(contracts, profiles, Options{ContextSnapshotRef: plan.ContextSnapshotRef, PolicyHash: "sha256:changed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := policyPlanner.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonPolicyChanged {
		t.Fatalf("policy invalidation error=%v", err)
	}

	evidencePlanner, err := NewFromContracts(contracts, profiles, Options{ContextSnapshotRef: plan.ContextSnapshotRef, EvidenceHash: "sha256:changed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := evidencePlanner.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonEvidenceChanged {
		t.Fatalf("evidence invalidation error=%v", err)
	}
}

func TestPlanExpiredCannotValidateForExecution(t *testing.T) {
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	contracts, profiles := loadExamples(t)
	p, err := NewFromContracts(contracts, profiles, Options{
		Now:     func() time.Time { return now },
		PlanTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(exampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidatePlan(plan, now.Add(time.Minute)); reasonCode(err) != domain.ReasonPlanExpired {
		t.Fatalf("expired plan validation=%v", err)
	}
}

func TestPlanRejectsHashTampering(t *testing.T) {
	contracts, profiles := loadExamples(t)
	p, err := NewFromContracts(contracts, profiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(exampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	plan.Phases[0].Steps[0].DesiredVersion = "tampered"
	if err := p.ValidatePlan(plan, time.Now()); reasonCode(err) != domain.ReasonPlanHashMismatch {
		t.Fatalf("tampered plan validation=%v", err)
	}
}

func TestNewRejectsUnknownDependency(t *testing.T) {
	_, err := New([]domain.Service{{Name: "a", Dependencies: []string{"missing"}}})
	if err == nil {
		t.Fatal("expected unknown dependency error")
	}
}

func TestPlanFiveHundredServicesFiveThousandEdges(t *testing.T) {
	contracts, profiles, request := generatedPlanningInput(500, 5000)
	started := time.Now()
	p, err := NewFromContracts(contracts, profiles, Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(request)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Second {
		t.Fatalf("planning took %s, want <10s", elapsed)
	}
	steps := 0
	for _, phase := range plan.Phases {
		steps += len(phase.Steps)
	}
	if steps != 500 {
		t.Fatalf("steps=%d, want 500", steps)
	}
}

func BenchmarkPlanFiveHundredServicesFiveThousandEdges(b *testing.B) {
	contracts, profiles, request := generatedPlanningInput(500, 5000)
	p, err := NewFromContracts(contracts, profiles, Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := p.Plan(request); err != nil {
			b.Fatal(err)
		}
	}
}

func loadExamples(t *testing.T) ([]contract.ServiceContract, []profile.ReleaseProfile) {
	t.Helper()
	contracts, err := contract.LoadDir("../../examples/contracts")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.LoadDir("../../examples/profiles")
	if err != nil {
		t.Fatal(err)
	}
	return contracts, profiles
}

func loadContract(t *testing.T, path string) contract.ServiceContract {
	t.Helper()
	loaded, err := contract.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func exampleRequest() domain.ReleaseRequest {
	return domain.ReleaseRequest{
		ReleaseVersion: "release-1",
		Environment:    domain.EnvironmentStaging,
		Changes: []domain.Change{
			{Service: "identity-api", DesiredVersion: "identity"},
			{Service: "payment-api", DesiredVersion: "payment"},
		},
	}
}

func reasonCode(err error) domain.ReasonCode {
	code, _ := domain.ReasonOf(err)
	return code
}

func generatedPlanningInput(serviceCount, edgeCount int) ([]contract.ServiceContract, []profile.ReleaseProfile, domain.ReleaseRequest) {
	releaseProfile := profile.ReleaseProfile{
		APIVersion: profile.APIVersion,
		Kind:       profile.Kind,
		Metadata:   profile.Metadata{Name: "benchmark"},
		Spec: profile.Spec{
			RequiredCapabilities: []domain.Capability{domain.CapabilityDeploy},
			Deployment:           profile.Deployment{Strategy: "benchmark"},
			Verification:         profile.Verification{Required: true, ObservationWindow: "1s"},
			Rollback:             profile.Rollback{Mode: "unsupported"},
		},
	}
	contracts := make([]contract.ServiceContract, serviceCount)
	changes := make([]domain.Change, serviceCount)
	remainingEdges := edgeCount
	for index := range serviceCount {
		name := fmt.Sprintf("service-%03d", index)
		dependencies := make([]domain.Dependency, 0)
		for dependencyIndex := 0; dependencyIndex < index && remainingEdges > 0; dependencyIndex++ {
			dependencies = append(dependencies, domain.Dependency{
				Service: fmt.Sprintf("service-%03d", dependencyIndex),
				Type:    domain.DependencyRolloutOrder,
			})
			remainingEdges--
		}
		contracts[index] = contract.ServiceContract{
			APIVersion: contract.APIVersion,
			Kind:       contract.Kind,
			Metadata: contract.Metadata{
				Name:       name,
				Owner:      "benchmark-team",
				Repository: "example/benchmark",
				RiskTier:   "low",
			},
			Environments: map[string]contract.EnvironmentContract{
				string(domain.EnvironmentStaging): {
					ReleaseProfile: "benchmark",
					RunnerGroup:    "benchmark",
					Capabilities:   []domain.Capability{domain.CapabilityDeploy},
				},
			},
			Dependencies: dependencies,
			Verification: contract.Verification{
				Profile: "benchmark", ObservationWindow: "1s",
				Checks: []contract.VerificationCheck{{Type: "health", Threshold: "healthy"}},
			},
			Rollback: contract.Rollback{Capability: "unsupported", Strategy: "none", Deadline: "1s"},
		}
		changes[index] = domain.Change{Service: name, DesiredVersion: fmt.Sprintf("sha-%03d", index)}
	}
	if remainingEdges != 0 {
		panic(fmt.Sprintf("could not generate requested edges: %d remaining", remainingEdges))
	}
	return contracts, []profile.ReleaseProfile{releaseProfile}, domain.ReleaseRequest{
		ReleaseVersion: "benchmark",
		Environment:    domain.EnvironmentStaging,
		Changes:        changes,
	}
}

func TestProductionProfileRequiresVerification(t *testing.T) {
	for _, environment := range []domain.Environment{domain.EnvironmentProduction, domain.EnvironmentStaging} {
		for _, mode := range []string{"false", "omitted", "true"} {
			required := mode == "true"
			t.Run(fmt.Sprintf("%s/required=%s", environment, mode), func(t *testing.T) {
				contracts, profiles := loadExamples(t)
				for i := range profiles {
					profiles[i].Spec.Verification.Required = required
					profiles[i].ContentHash = ""
					if mode == "omitted" {
						data, err := json.Marshal(profiles[i])
						if err != nil {
							t.Fatal(err)
						}
						data = bytes.ReplaceAll(data, []byte(`"required":false,`), nil)
						profiles[i], err = profile.Decode(data)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				p, err := NewFromContracts(contracts, profiles, Options{})
				if err != nil {
					t.Fatal(err)
				}
				plan, err := p.Plan(domain.ReleaseRequest{ReleaseVersion: "release", Environment: environment, Changes: []domain.Change{{Service: "identity-api", DesiredVersion: "sha"}}})
				if environment == domain.EnvironmentProduction && !required {
					if err == nil {
						t.Fatal("production accepted verification omission/false")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if plan.Phases[0].Steps[0].VerificationRequired != required {
					t.Fatal("verification setting changed")
				}
			})
		}
	}
}
