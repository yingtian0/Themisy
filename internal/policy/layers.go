package policy

import (
	"context"
	"sort"
	"themisy/internal/domain"
)

// Every earlier layer is evaluated independently of later modules. A later
// Rego rule therefore cannot erase an upper deny, approval, role or reason.
func compileLayered(ctx context.Context, bundle Bundle, compile func(context.Context, Bundle) (*OPA, error)) (*OPA, error) {
	baseline, err := NewMandatoryEngine(ctx)
	if err != nil {
		return nil, err
	}
	result := &OPA{bundleHash: bundle.Hash, bundleVersion: bundle.Version, components: []*OPA{baseline.evaluator.(*OPA)}}
	cumulative := []Module{}
	for _, layer := range []Layer{LayerPlatform, LayerEnvironment, LayerTeam, LayerService} {
		added := false
		for _, module := range normalizedBundle(bundle).Modules {
			if module.Layer == layer {
				cumulative = append(cumulative, module)
				added = true
			}
		}
		if !added {
			continue
		}
		prefix := bundle
		prefix.Modules = append([]Module(nil), cumulative...)
		compiled, err := compile(ctx, prefix)
		if err != nil {
			return nil, err
		}
		result.components = append(result.components, compiled)
	}
	return result, nil
}
func (o *OPA) evaluateLayers(ctx context.Context, input Input) (Evaluation, error) {
	hash, err := InputHash(input)
	if err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{Decision: domain.DecisionAllow, InputHash: hash, PolicyHash: o.bundleHash, PolicyVersion: o.bundleVersion}
	reasons, roles := map[string]bool{}, map[string]bool{}
	for _, component := range o.components {
		value, err := component.Evaluate(ctx, input)
		if err != nil {
			return Evaluation{}, err
		}
		if value.Decision == domain.DecisionDeny {
			result.Decision = domain.DecisionDeny
		} else if value.Decision == domain.DecisionRequireApproval && result.Decision != domain.DecisionDeny {
			result.Decision = domain.DecisionRequireApproval
		}
		for _, reason := range value.Reasons {
			reasons[reason] = true
		}
		for _, role := range value.RequiredRoles {
			roles[role] = true
		}
	}
	if result.Decision != domain.DecisionAllow {
		delete(reasons, "POLICY_ALLOW")
	}
	for reason := range reasons {
		result.Reasons = append(result.Reasons, reason)
	}
	sort.Strings(result.Reasons)
	if result.Decision == domain.DecisionRequireApproval {
		for role := range roles {
			result.RequiredRoles = append(result.RequiredRoles, role)
		}
		sort.Strings(result.RequiredRoles)
	}
	return result, nil
}
