package scheduler

import (
	"fmt"
	"sort"
	"sync"
)

// Step is the immutable scheduling projection of a planned release step.
// Identity and failure-domain data must come from trusted contracts and the
// authenticated request, never from an interface-provided concurrency hint.
type Step struct {
	ID             string   `json:"id" yaml:"id"`
	Phase          int      `json:"phase" yaml:"phase"`
	Tenant         string   `json:"tenant" yaml:"tenant"`
	Environment    string   `json:"environment" yaml:"environment"`
	Region         string   `json:"region" yaml:"region"`
	Cluster        string   `json:"cluster" yaml:"cluster"`
	Team           string   `json:"team" yaml:"team"`
	RiskTier       string   `json:"risk_tier" yaml:"risk_tier"`
	RunnerGroup    string   `json:"runner_group" yaml:"runner_group"`
	FailureDomains []string `json:"failure_domains,omitempty" yaml:"failure_domains,omitempty"`
	Dependencies   []string `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
}

// Limits is the complete set of concurrency caps. Missing dimension entries
// use DefaultPerDimension. A value below one is fail-closed, not unlimited.
type Limits struct {
	TenantGlobal        int            `json:"tenant_global" yaml:"tenant_global"`
	DefaultPerDimension int            `json:"default_per_dimension" yaml:"default_per_dimension"`
	Environment         map[string]int `json:"environment,omitempty" yaml:"environment,omitempty"`
	Region              map[string]int `json:"region,omitempty" yaml:"region,omitempty"`
	Cluster             map[string]int `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	Team                map[string]int `json:"team,omitempty" yaml:"team,omitempty"`
	RiskTier            map[string]int `json:"risk_tier,omitempty" yaml:"risk_tier,omitempty"`
	SharedFailureDomain int            `json:"shared_failure_domain" yaml:"shared_failure_domain"`
}

func DefaultLimits() Limits {
	return Limits{TenantGlobal: 20, DefaultPerDimension: 20, SharedFailureDomain: 1}
}

func (l Limits) Validate() error {
	if l.TenantGlobal < 1 || l.DefaultPerDimension < 1 || l.SharedFailureDomain < 1 {
		return fmt.Errorf("all default concurrency limits must be positive")
	}
	for name, values := range map[string]map[string]int{
		"environment": l.Environment, "region": l.Region, "cluster": l.Cluster,
		"team": l.Team, "risk_tier": l.RiskTier,
	} {
		for key, value := range values {
			if key == "" || value < 1 {
				return fmt.Errorf("%s concurrency limit %q must be positive", name, key)
			}
		}
	}
	return nil
}

type Dimension string

const (
	DimensionTenant        Dimension = "tenant/global"
	DimensionEnvironment   Dimension = "environment"
	DimensionRegion        Dimension = "region"
	DimensionCluster       Dimension = "cluster"
	DimensionTeam          Dimension = "team"
	DimensionRiskTier      Dimension = "risk-tier"
	DimensionFailureDomain Dimension = "shared-failure-domain"
	DimensionService       Dimension = "service+environment"
)

type Blocked struct {
	Dimension Dimension `json:"dimension"`
	Key       string    `json:"key"`
	Limit     int       `json:"limit"`
}

// Constraint is one shared capacity dimension used by the durable coordinator.
// Keys are canonicalized by the scheduler and must not come from request hints.
type Constraint struct {
	Dimension Dimension `json:"dimension"`
	Key       string    `json:"key"`
	Limit     int       `json:"limit"`
}

type Reservation struct {
	step Step
	keys []budgetKey
}

type budgetKey struct {
	dimension Dimension
	key       string
	limitKey  string
}

// Budget atomically applies the intersection of every configured limit.
type Budget struct {
	mu     sync.Mutex
	limits Limits
	active map[budgetKey]int
}

func NewBudget(limits Limits) (*Budget, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Budget{limits: limits, active: make(map[budgetKey]int)}, nil
}

func (b *Budget) TryAcquire(step Step) (Reservation, []Blocked) {
	keys := keysFor(step)
	b.mu.Lock()
	defer b.mu.Unlock()
	blocked := make([]Blocked, 0)
	for _, key := range keys {
		limit := b.limit(key)
		if b.active[key] >= limit {
			blocked = append(blocked, Blocked{Dimension: key.dimension, Key: key.key, Limit: limit})
		}
	}
	if len(blocked) > 0 {
		return Reservation{}, blocked
	}
	for _, key := range keys {
		b.active[key]++
	}
	return Reservation{step: step, keys: keys}, nil
}

func (b *Budget) Release(reservation Reservation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, key := range reservation.keys {
		if b.active[key] <= 1 {
			delete(b.active, key)
		} else {
			b.active[key]--
		}
	}
}

func (b *Budget) limit(key budgetKey) int {
	switch key.dimension {
	case DimensionTenant:
		return b.limits.TenantGlobal
	case DimensionEnvironment:
		return configuredLimit(b.limits.Environment, key.limitKey, b.limits.DefaultPerDimension)
	case DimensionRegion:
		return configuredLimit(b.limits.Region, key.limitKey, b.limits.DefaultPerDimension)
	case DimensionCluster:
		return configuredLimit(b.limits.Cluster, key.limitKey, b.limits.DefaultPerDimension)
	case DimensionTeam:
		return configuredLimit(b.limits.Team, key.limitKey, b.limits.DefaultPerDimension)
	case DimensionRiskTier:
		return configuredLimit(b.limits.RiskTier, key.limitKey, b.limits.DefaultPerDimension)
	case DimensionFailureDomain:
		return b.limits.SharedFailureDomain
	case DimensionService:
		return 1
	default:
		return 0
	}
}

func configuredLimit(values map[string]int, key string, fallback int) int {
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

func keysFor(step Step) []budgetKey {
	tenant := step.Tenant
	if tenant == "" {
		tenant = "default"
	}
	keys := []budgetKey{
		{DimensionTenant, tenant, tenant},
		{DimensionEnvironment, tenant + "/" + step.Environment, step.Environment},
		{DimensionRegion, tenant + "/" + step.Environment + "/" + step.Region, step.Region},
		{DimensionCluster, tenant + "/" + step.Environment + "/" + step.Region + "/" + step.Cluster, step.Cluster},
		{DimensionTeam, tenant + "/" + step.Team, step.Team},
		{DimensionRiskTier, tenant + "/" + step.RiskTier, step.RiskTier},
		{DimensionService, tenant + "/" + step.Environment + "/" + step.ID, step.ID},
	}
	domains := append([]string(nil), step.FailureDomains...)
	sort.Strings(domains)
	for _, failureDomain := range domains {
		keys = append(keys, budgetKey{DimensionFailureDomain, tenant + "/" + step.Environment + "/" + failureDomain, failureDomain})
	}
	return keys
}

// ConstraintsFor returns the complete, deterministic capacity intersection
// for a step. Duplicate failure-domain names collapse to one constraint.
func ConstraintsFor(step Step, limits Limits) ([]Constraint, error) {
	budget, err := NewBudget(limits)
	if err != nil {
		return nil, err
	}
	keys := keysFor(step)
	seen := make(map[budgetKey]bool, len(keys))
	constraints := make([]Constraint, 0, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		limit := budget.limit(key)
		if limit < 1 {
			return nil, fmt.Errorf("invalid concurrency limit for %s/%s", key.dimension, key.key)
		}
		constraints = append(constraints, Constraint{Dimension: key.dimension, Key: key.key, Limit: limit})
	}
	sort.Slice(constraints, func(i, j int) bool {
		if constraints[i].Dimension == constraints[j].Dimension {
			return constraints[i].Key < constraints[j].Key
		}
		return constraints[i].Dimension < constraints[j].Dimension
	})
	return constraints, nil
}
