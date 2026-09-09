package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"

	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/profile"
)

const PlanAPIVersion = "execution.themisy.io/v1alpha1"

var (
	ErrNoChanges       = errors.New("at least one change is required")
	ErrDependencyCycle = errors.New("dependency cycle detected")
)

type Options struct {
	Now                func() time.Time
	PlanTTL            time.Duration
	ContextSnapshotRef string
	PolicyHash         string
	EvidenceHash       string
}

type serviceDefinition struct {
	service      domain.Service
	dependencies []domain.Dependency
	environments map[domain.Environment]environmentDefinition
	contractHash string
}

type environmentDefinition struct {
	profile     string
	runnerGroup string
}

type Planner struct {
	services           map[string]domain.Service
	definitions        map[string]serviceDefinition
	profiles           map[string]profile.ReleaseProfile
	contractHashes     map[string]string
	profileHashes      map[string]string
	contextSnapshotRef string
	policyHash         string
	evidenceHash       string
	now                func() time.Time
	planTTL            time.Duration
}

func New(services []domain.Service) (*Planner, error) {
	definitions := make(map[string]serviceDefinition, len(services))
	byName := make(map[string]domain.Service, len(services))
	contractHashes := make(map[string]string, len(services))
	for _, service := range services {
		if service.Name == "" {
			return nil, errors.New("service name is required")
		}
		if _, exists := byName[service.Name]; exists {
			return nil, fmt.Errorf("duplicate service %q", service.Name)
		}
		dependencies := make([]domain.Dependency, 0, len(service.Dependencies))
		for _, dependency := range service.Dependencies {
			dependencies = append(dependencies, domain.Dependency{
				Service: dependency,
				Type:    domain.DependencyRolloutOrder,
			})
		}
		hash, err := hashValue(struct {
			Name         string              `json:"name"`
			Owner        string              `json:"owner"`
			Repository   string              `json:"repository"`
			Dependencies []domain.Dependency `json:"dependencies"`
		}{service.Name, service.OwnerTeam, service.Repository, sortedDependencies(dependencies)})
		if err != nil {
			return nil, err
		}
		definition := serviceDefinition{
			service:      service,
			dependencies: sortedDependencies(dependencies),
			environments: map[domain.Environment]environmentDefinition{
				domain.EnvironmentStaging:    {profile: "legacy", runnerGroup: "legacy-staging"},
				domain.EnvironmentProduction: {profile: "legacy", runnerGroup: "legacy-production"},
			},
			contractHash: hash,
		}
		byName[service.Name] = service
		definitions[service.Name] = definition
		contractHashes[service.Name] = hash
	}
	for _, definition := range definitions {
		for _, dependency := range definition.dependencies {
			if _, exists := byName[dependency.Service]; !exists {
				return nil, fmt.Errorf("service %q references unknown dependency %q", definition.service.Name, dependency.Service)
			}
		}
	}
	legacyProfileHash, err := hashValue(map[string]string{"name": "legacy"})
	if err != nil {
		return nil, err
	}
	profileHashes := map[string]string{"legacy": legacyProfileHash}
	return newPlanner(byName, definitions, nil, contractHashes, profileHashes, Options{}), nil
}

func NewFromContracts(contracts []contract.ServiceContract, profiles []profile.ReleaseProfile, options Options) (*Planner, error) {
	if len(contracts) == 0 {
		return nil, domain.NewReasonError(domain.ReasonInvalidContract, "contracts", "at least one service contract is required", nil)
	}
	profileByName := make(map[string]profile.ReleaseProfile, len(profiles))
	profileHashes := make(map[string]string, len(profiles))
	for _, releaseProfile := range profiles {
		if err := profile.Validate(releaseProfile); err != nil {
			return nil, err
		}
		name := releaseProfile.Metadata.Name
		if _, duplicate := profileByName[name]; duplicate {
			return nil, domain.NewReasonError(domain.ReasonInvalidProfile, "metadata.name", fmt.Sprintf("duplicate profile %q", name), nil)
		}
		hash, err := profile.Hash(releaseProfile)
		if err != nil {
			return nil, err
		}
		if releaseProfile.ContentHash != "" && releaseProfile.ContentHash != hash {
			return nil, domain.NewReasonError(domain.ReasonInvalidProfile, "content_hash", fmt.Sprintf("profile %q content changed after hashing", name), nil)
		}
		releaseProfile.ContentHash = hash
		profileByName[name] = releaseProfile
		profileHashes[name] = hash
	}

	definitions := make(map[string]serviceDefinition, len(contracts))
	services := make(map[string]domain.Service, len(contracts))
	contractHashes := make(map[string]string, len(contracts))
	for _, serviceContract := range contracts {
		if err := contract.Validate(serviceContract); err != nil {
			return nil, err
		}
		name := serviceContract.Metadata.Name
		if _, duplicate := definitions[name]; duplicate {
			return nil, domain.NewReasonError(domain.ReasonInvalidContract, "metadata.name", fmt.Sprintf("duplicate service %q", name), nil)
		}
		hash, err := contract.Hash(serviceContract)
		if err != nil {
			return nil, err
		}
		if serviceContract.ContentHash != "" && serviceContract.ContentHash != hash {
			return nil, domain.NewReasonError(domain.ReasonInvalidContract, "content_hash", fmt.Sprintf("service %q content changed after hashing", name), nil)
		}
		environments := make(map[domain.Environment]environmentDefinition, len(serviceContract.Environments))
		for name, environment := range serviceContract.Environments {
			releaseProfile, ok := profileByName[environment.ReleaseProfile]
			if !ok {
				return nil, domain.NewReasonError(domain.ReasonUnknownProfile, "environments."+name+".releaseProfile", fmt.Sprintf("profile %q does not exist", environment.ReleaseProfile), nil)
			}
			if missing := missingCapabilities(environment.Capabilities, releaseProfile.Spec.RequiredCapabilities); len(missing) > 0 {
				return nil, domain.NewReasonError(domain.ReasonMissingCapability, "environments."+name+".capabilities", fmt.Sprintf("profile %q requires %v", environment.ReleaseProfile, missing), nil)
			}
			environments[domain.Environment(name)] = environmentDefinition{
				profile:     environment.ReleaseProfile,
				runnerGroup: environment.RunnerGroup,
			}
		}
		dependencies := sortedDependencies(serviceContract.Dependencies)
		ordered := make([]string, 0, len(dependencies))
		failureDomains := make([]string, 0)
		for _, dependency := range dependencies {
			if dependency.Service != "" && dependency.Type.EnforcesRolloutOrder() {
				ordered = append(ordered, dependency.Service)
			}
			if dependency.Type == domain.DependencySharedFailureDomain && dependency.Resource != "" {
				failureDomains = append(failureDomains, dependency.Resource)
			}
		}
		runnerGroups := make(map[domain.Environment]string, len(environments))
		for environment, definition := range environments {
			runnerGroups[environment] = definition.runnerGroup
		}
		service := domain.Service{
			ID:              name,
			Name:            name,
			OwnerTeam:       serviceContract.Metadata.Owner,
			RiskTier:        serviceContract.Metadata.RiskTier,
			Repository:      serviceContract.Metadata.Repository,
			Dependencies:    ordered,
			RunnerGroups:    runnerGroups,
			FailureDomains:  failureDomains,
			MetadataVersion: serviceContract.APIVersion,
		}
		services[name] = service
		definitions[name] = serviceDefinition{
			service:      service,
			dependencies: dependencies,
			environments: environments,
			contractHash: hash,
		}
		contractHashes[name] = hash
	}
	for name, definition := range definitions {
		for _, dependency := range definition.dependencies {
			if dependency.Service == "" {
				continue
			}
			if _, exists := definitions[dependency.Service]; !exists {
				return nil, domain.NewReasonError(domain.ReasonUnknownDependency, "services."+name+".dependencies", fmt.Sprintf("service %q does not exist", dependency.Service), nil)
			}
		}
	}
	globalPhases, err := validateGlobalOrderingGraph(definitions)
	if err != nil {
		return nil, err
	}
	for name, phase := range globalPhases {
		service := services[name]
		service.ReleasePhase = phase
		services[name] = service
		definition := definitions[name]
		definition.service = service
		definitions[name] = definition
	}
	return newPlanner(services, definitions, profileByName, contractHashes, profileHashes, options), nil
}

func newPlanner(services map[string]domain.Service, definitions map[string]serviceDefinition, profiles map[string]profile.ReleaseProfile, contractHashes, profileHashes map[string]string, options Options) *Planner {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PlanTTL <= 0 {
		options.PlanTTL = 15 * time.Minute
	}
	if options.ContextSnapshotRef == "" {
		contextHash, _ := hashValue(struct {
			Contracts    map[string]string `json:"contracts"`
			Profiles     map[string]string `json:"profiles"`
			PolicyHash   string            `json:"policy_hash"`
			EvidenceHash string            `json:"evidence_hash"`
		}{contractHashes, profileHashes, options.PolicyHash, options.EvidenceHash})
		options.ContextSnapshotRef = "content:" + contextHash
	}
	return &Planner{
		services:           services,
		definitions:        definitions,
		profiles:           profiles,
		contractHashes:     maps.Clone(contractHashes),
		profileHashes:      maps.Clone(profileHashes),
		contextSnapshotRef: options.ContextSnapshotRef,
		policyHash:         options.PolicyHash,
		evidenceHash:       options.EvidenceHash,
		now:                options.Now,
		planTTL:            options.PlanTTL,
	}
}

func (p *Planner) Services() []domain.Service {
	services := make([]domain.Service, 0, len(p.services))
	for _, service := range p.services {
		service.Dependencies = append([]string(nil), service.Dependencies...)
		service.FailureDomains = append([]string(nil), service.FailureDomains...)
		service.RunnerGroups = maps.Clone(service.RunnerGroups)
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services
}

func (p *Planner) Plan(request domain.ReleaseRequest) (domain.ReleasePlan, error) {
	return p.PlanIntent(IntentFromLegacy(request))
}

func IntentFromLegacy(request domain.ReleaseRequest) domain.ReleaseIntent {
	return domain.ReleaseIntent{
		APIVersion:        domain.ReleaseIntentAPIVersion,
		Kind:              domain.ReleaseIntentKind,
		RequestID:         request.RequestID,
		ReleaseVersion:    request.ReleaseVersion,
		TenantID:          request.TenantID,
		Environment:       request.Environment,
		Region:            request.Region,
		Cluster:           request.Cluster,
		RequestedBy:       request.RequestedBy,
		SubjectType:       request.SubjectType,
		SubjectIssuer:     request.SubjectIssuer,
		UserIdentityProof: request.UserIdentityProof,
		DelegationRef:     request.DelegationRef,
		Agent:             request.Agent,
		Changes:           append([]domain.Change(nil), request.Changes...),
	}
}

func LegacyRequestFromIntent(intent domain.ReleaseIntent) domain.ReleaseRequest {
	return domain.ReleaseRequest{
		RequestID:         intent.RequestID,
		ReleaseVersion:    intent.ReleaseVersion,
		TenantID:          intent.TenantID,
		Environment:       intent.Environment,
		Region:            intent.Region,
		Cluster:           intent.Cluster,
		RequestedBy:       intent.RequestedBy,
		SubjectType:       intent.SubjectType,
		SubjectIssuer:     intent.SubjectIssuer,
		UserIdentityProof: intent.UserIdentityProof,
		DelegationRef:     intent.DelegationRef,
		Agent:             intent.Agent,
		Changes:           append([]domain.Change(nil), intent.Changes...),
	}
}

func (p *Planner) PlanIntent(intent domain.ReleaseIntent) (domain.ReleasePlan, error) {
	if intent.APIVersion != domain.ReleaseIntentAPIVersion {
		return domain.ReleasePlan{}, domain.NewReasonError(domain.ReasonUnsupportedSchemaVersion, "api_version", fmt.Sprintf("got %q, want %q", intent.APIVersion, domain.ReleaseIntentAPIVersion), nil)
	}
	if intent.Kind != domain.ReleaseIntentKind {
		return domain.ReleasePlan{}, fmt.Errorf("unsupported intent kind %q", intent.Kind)
	}
	if len(intent.Changes) == 0 {
		return domain.ReleasePlan{}, ErrNoChanges
	}
	if intent.Environment != domain.EnvironmentStaging && intent.Environment != domain.EnvironmentProduction {
		return domain.ReleasePlan{}, fmt.Errorf("unsupported environment %q", intent.Environment)
	}

	changes := make(map[string]domain.Change, len(intent.Changes))
	for _, change := range intent.Changes {
		definition, exists := p.definitions[change.Service]
		if !exists {
			return domain.ReleasePlan{}, fmt.Errorf("unknown service %q", change.Service)
		}
		if _, exists := definition.environments[intent.Environment]; !exists {
			return domain.ReleasePlan{}, fmt.Errorf("service %q does not declare environment %q", change.Service, intent.Environment)
		}
		if change.DesiredVersion == "" {
			return domain.ReleasePlan{}, fmt.Errorf("desired version is required for %q", change.Service)
		}
		if _, duplicate := changes[change.Service]; duplicate {
			return domain.ReleasePlan{}, fmt.Errorf("duplicate change for %q", change.Service)
		}
		changes[change.Service] = change
	}

	indegree := make(map[string]int, len(changes))
	children := make(map[string][]string, len(changes))
	phase := make(map[string]int, len(changes))
	for name := range changes {
		definition := p.definitions[name]
		phase[name] = definition.service.ReleasePhase
		for _, dependency := range definition.dependencies {
			if dependency.Service == "" || !dependency.Type.EnforcesRolloutOrder() {
				continue
			}
			if _, selected := changes[dependency.Service]; selected {
				indegree[name]++
				children[dependency.Service] = append(children[dependency.Service], name)
			}
		}
	}

	ready := make([]string, 0, len(changes))
	for name := range changes {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	processed := 0
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		processed++
		sort.Strings(children[name])
		for _, child := range children[name] {
			if phase[child] <= phase[name] {
				phase[child] = phase[name] + 1
			}
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if processed != len(changes) {
		return domain.ReleasePlan{}, domain.NewReasonError(domain.ReasonDependencyCycle, "changes", "selected dependencies contain a cycle", ErrDependencyCycle)
	}

	byPhase := make(map[int][]domain.PlanStep)
	phaseNumbers := make([]int, 0)
	for name, change := range changes {
		definition := p.definitions[name]
		environment := definition.environments[intent.Environment]
		changeHash, err := hashValue(change)
		if err != nil {
			return domain.ReleasePlan{}, err
		}
		if _, exists := byPhase[phase[name]]; !exists {
			phaseNumbers = append(phaseNumbers, phase[name])
		}
		step := domain.PlanStep{
			Service:              name,
			DesiredVersion:       change.DesiredVersion,
			Phase:                phase[name],
			Profile:              environment.profile,
			Dependencies:         sortedDependencies(definition.dependencies),
			ContractHash:         definition.contractHash,
			ProfileHash:          p.profileHashes[environment.profile],
			ChangeHash:           changeHash,
			VerificationRequired: true,
			ObservationWindow:    "1m",
			RollbackMode:         domain.RollbackAutomatic,
			Scheduling: domain.SchedulingContext{
				TenantID:       normalizedValue(intent.TenantID, "default"),
				Environment:    intent.Environment,
				Region:         normalizedValue(intent.Region, "global"),
				Cluster:        normalizedValue(intent.Cluster, environment.runnerGroup),
				Team:           definition.service.OwnerTeam,
				RiskTier:       normalizedValue(definition.service.RiskTier, "medium"),
				RunnerGroup:    environment.runnerGroup,
				FailureDomains: append([]string(nil), definition.service.FailureDomains...),
			},
		}
		if releaseProfile, ok := p.profiles[environment.profile]; ok {
			step.RequiredCapabilities = sortedCapabilities(releaseProfile.Spec.RequiredCapabilities)
			step.VerificationRequired = releaseProfile.Spec.Verification.Required
			step.ObservationWindow = releaseProfile.Spec.Verification.ObservationWindow
			step.RollbackMode = domain.RollbackMode(releaseProfile.Spec.Rollback.Mode)
		}
		byPhase[phase[name]] = append(byPhase[phase[name]], step)
	}
	sort.Ints(phaseNumbers)
	now := p.now().UTC()
	plan := domain.ReleasePlan{
		APIVersion:         PlanAPIVersion,
		ReleaseVersion:     intent.ReleaseVersion,
		Environment:        intent.Environment,
		ContractHashes:     maps.Clone(p.contractHashes),
		ProfileHashes:      maps.Clone(p.profileHashes),
		ContextSnapshotRef: p.contextSnapshotRef,
		PolicyHash:         p.policyHash,
		EvidenceHash:       p.evidenceHash,
		CreatedAt:          now,
		ExpiresAt:          now.Add(p.planTTL),
	}
	for _, number := range phaseNumbers {
		steps := byPhase[number]
		sort.Slice(steps, func(i, j int) bool { return steps[i].Service < steps[j].Service })
		plan.Phases = append(plan.Phases, domain.PlanPhase{Number: number, Steps: steps})
	}
	hash, err := planHash(plan)
	if err != nil {
		return domain.ReleasePlan{}, err
	}
	plan.Hash = hash
	plan.PlanHash = hash
	plan.ID = "plan_" + hash[len("sha256:"):len("sha256:")+24]
	return plan, nil
}

func normalizedValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (p *Planner) ValidatePlan(plan domain.ReleasePlan, now time.Time) error {
	hash, err := planHash(plan)
	if err != nil {
		return err
	}
	if plan.PlanHash == "" || plan.Hash == "" || plan.PlanHash != plan.Hash || plan.PlanHash != hash {
		return domain.NewReasonError(domain.ReasonPlanHashMismatch, "plan_hash", "plan content does not match its hash", nil)
	}
	if plan.ExpiresAt.IsZero() || !now.UTC().Before(plan.ExpiresAt.UTC()) {
		return domain.NewReasonError(domain.ReasonPlanExpired, "expires_at", "plan is expired", nil)
	}
	if !maps.Equal(plan.ContractHashes, p.contractHashes) {
		return domain.NewReasonError(domain.ReasonContractChanged, "contract_hashes", "service contracts changed after planning", nil)
	}
	if !maps.Equal(plan.ProfileHashes, p.profileHashes) {
		return domain.NewReasonError(domain.ReasonProfileChanged, "profile_hashes", "release profiles changed after planning", nil)
	}
	if plan.ContextSnapshotRef != p.contextSnapshotRef {
		return domain.NewReasonError(domain.ReasonContextChanged, "context_snapshot_ref", "context snapshot changed after planning", nil)
	}
	if plan.PolicyHash != p.policyHash {
		return domain.NewReasonError(domain.ReasonPolicyChanged, "policy_hash", "policy changed after planning", nil)
	}
	if plan.EvidenceHash != p.evidenceHash {
		return domain.NewReasonError(domain.ReasonEvidenceChanged, "evidence_hash", "evidence context changed after planning", nil)
	}
	return nil
}

func planHash(plan domain.ReleasePlan) (string, error) {
	canonical := struct {
		APIVersion         string             `json:"api_version"`
		ReleaseVersion     string             `json:"release_version"`
		Environment        domain.Environment `json:"environment"`
		Phases             []domain.PlanPhase `json:"phases"`
		ContractHashes     map[string]string  `json:"contract_hashes"`
		ProfileHashes      map[string]string  `json:"profile_hashes"`
		ContextSnapshotRef string             `json:"context_snapshot_ref"`
		PolicyHash         string             `json:"policy_hash"`
		EvidenceHash       string             `json:"evidence_hash"`
	}{
		APIVersion:         plan.APIVersion,
		ReleaseVersion:     plan.ReleaseVersion,
		Environment:        plan.Environment,
		Phases:             plan.Phases,
		ContractHashes:     plan.ContractHashes,
		ProfileHashes:      plan.ProfileHashes,
		ContextSnapshotRef: plan.ContextSnapshotRef,
		PolicyHash:         plan.PolicyHash,
		EvidenceHash:       plan.EvidenceHash,
	}
	return hashValue(canonical)
}

func validateGlobalOrderingGraph(definitions map[string]serviceDefinition) (map[string]int, error) {
	indegree := make(map[string]int, len(definitions))
	children := make(map[string][]string, len(definitions))
	phases := make(map[string]int, len(definitions))
	for name, definition := range definitions {
		for _, dependency := range definition.dependencies {
			if dependency.Service == "" || !dependency.Type.EnforcesRolloutOrder() {
				continue
			}
			indegree[name]++
			children[dependency.Service] = append(children[dependency.Service], name)
		}
	}
	ready := make([]string, 0, len(definitions))
	for name := range definitions {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	processed := 0
	for len(ready) > 0 {
		name := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		processed++
		for _, child := range children[name] {
			if phases[child] <= phases[name] {
				phases[child] = phases[name] + 1
			}
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if processed != len(definitions) {
		return nil, domain.NewReasonError(domain.ReasonDependencyCycle, "dependencies", "service contracts contain an ordering cycle", ErrDependencyCycle)
	}
	return phases, nil
}

func missingCapabilities(actual, required []domain.Capability) []domain.Capability {
	set := make(map[domain.Capability]struct{}, len(actual))
	for _, capability := range actual {
		set[capability] = struct{}{}
	}
	missing := make([]domain.Capability, 0)
	for _, capability := range required {
		if _, ok := set[capability]; !ok {
			missing = append(missing, capability)
		}
	}
	return sortedCapabilities(missing)
}

func sortedCapabilities(values []domain.Capability) []domain.Capability {
	result := append([]domain.Capability(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func sortedDependencies(values []domain.Dependency) []domain.Dependency {
	result := append([]domain.Dependency(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Type != right.Type {
			return left.Type < right.Type
		}
		if left.Service != right.Service {
			return left.Service < right.Service
		}
		if left.Resource != right.Resource {
			return left.Resource < right.Resource
		}
		if left.Condition != right.Condition {
			return left.Condition < right.Condition
		}
		return left.Constraint < right.Constraint
	})
	return result
}

func hashValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode canonical value: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
