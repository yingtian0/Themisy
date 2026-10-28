package policy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"themisy/internal/domain"
	policyfiles "themisy/policies"

	"github.com/open-policy-agent/opa/v1/rego"
)

var stableReasonCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

type OPA struct {
	components    []*OPA
	query         rego.PreparedEvalQuery
	bundleVersion string
	bundleHash    string
}

func (o *OPA) PolicyHash() string {
	if o == nil {
		return ""
	}
	return o.bundleHash
}

func NewMandatoryEngine(ctx context.Context) (*Engine, error) {
	baseline, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		return nil, err
	}
	bundle := Bundle{Version: "embedded-baseline-v1", Issuer: "embedded://themisy", Compatibility: []string{InputVersionV1Alpha1}, IssuedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), Modules: []Module{{Name: "main.rego", Layer: LayerPlatform, Source: string(baseline)}}}
	bundle.Hash, err = CalculateBundleHash(bundle)
	if err != nil {
		return nil, err
	}
	opa, err := newOPA(ctx, bundle)
	if err != nil {
		return nil, err
	}
	return NewEngine(opa), nil
}

func NewVerifiedOPA(ctx context.Context, bundle Bundle, issuer string, keys BundleKeyResolver, now time.Time) (*OPA, error) {
	if err := VerifyBundle(ctx, bundle, issuer, keys, now); err != nil {
		return nil, err
	}
	return compileLayered(ctx, bundle, newOPA)
}

func newOPA(ctx context.Context, bundle Bundle) (*OPA, error) {
	options := []func(*rego.Rego){rego.Query("data.themisy.authorization.decision"), rego.Capabilities(offlineCapabilities())}
	seenMandatory := false
	for _, module := range normalizedBundle(bundle).Modules {
		if module.Name == "" || module.Source == "" {
			return nil, fmt.Errorf("INVALID_POLICY_BUNDLE: empty module")
		}
		if module.Layer == LayerPlatform {
			seenMandatory = true
		}
		options = append(options, rego.Module(string(module.Layer)+"/"+module.Name, module.Source))
	}
	if !seenMandatory {
		return nil, fmt.Errorf("INVALID_POLICY_BUNDLE: platform mandatory layer is required")
	}
	query, err := rego.New(options...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile policy bundle: %w", err)
	}
	return &OPA{query: query, bundleVersion: bundle.Version, bundleHash: bundle.Hash}, nil
}

func (o *OPA) Evaluate(ctx context.Context, input Input) (Evaluation, error) {
	if o == nil {
		return Evaluation{}, fmt.Errorf("POLICY_UNAVAILABLE")
	}
	input = NormalizeInput(input)
	if len(o.components) > 0 {
		return o.evaluateLayers(ctx, input)
	}
	results, err := o.query.Eval(ctx, rego.EvalInput(input))
	if err != nil || len(results) != 1 || len(results[0].Expressions) != 1 {
		return Evaluation{}, fmt.Errorf("POLICY_UNAVAILABLE: invalid OPA result: %w", err)
	}
	object, ok := results[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return Evaluation{}, fmt.Errorf("POLICY_UNAVAILABLE: result is not an object")
	}
	allowed, ok := object["allow"].(bool)
	if !ok {
		return Evaluation{}, fmt.Errorf("POLICY_UNAVAILABLE: allow is missing")
	}
	reasons, err := stringSlice(object["reasons"])
	if err != nil {
		return Evaluation{}, err
	}
	sort.Strings(reasons)
	decision := domain.DecisionDeny
	requireApproval, _ := object["require_approval"].(bool)
	if allowed {
		decision = domain.DecisionAllow
		if len(reasons) == 0 {
			reasons = []string{"POLICY_ALLOW"}
		}
	} else if requireApproval {
		decision = domain.DecisionRequireApproval
	}
	hash, err := InputHash(input)
	if err != nil {
		return Evaluation{}, err
	}
	requiredRoles, err := optionalStringSlice(object["required_roles"])
	if err != nil {
		return Evaluation{}, err
	}
	if decision != domain.DecisionRequireApproval {
		requiredRoles = nil
	}
	return Evaluation{Decision: decision, Reasons: reasons, RequiredRoles: requiredRoles, InputHash: hash, PolicyVersion: o.bundleVersion, PolicyHash: o.bundleHash}, nil
}

func stringSlice(value any) ([]string, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("POLICY_UNAVAILABLE: reasons are invalid")
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || !stableReasonCode.MatchString(text) {
			return nil, fmt.Errorf("POLICY_UNAVAILABLE: reason is invalid")
		}
		result = append(result, text)
	}
	return result, nil
}

func optionalStringSlice(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("POLICY_UNAVAILABLE: required roles are invalid")
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("POLICY_UNAVAILABLE: required role is invalid")
		}
		result = append(result, text)
	}
	sort.Strings(result)
	return result, nil
}
