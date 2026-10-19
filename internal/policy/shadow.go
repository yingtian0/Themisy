package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
)

const ShadowReportVersion = "themisy.policy-shadow/v1"
const ShadowReportMode = "OFFLINE_SHADOW_ONLY"
const maximumShadowCases = 256

type ShadowCase struct {
	ID    string `json:"id"`
	Input Input  `json:"input"`
}

type ShadowComparison struct {
	CaseID               string     `json:"case_id"`
	InputHash            string     `json:"input_hash"`
	Current              Evaluation `json:"current"`
	Candidate            Evaluation `json:"candidate"`
	DecisionChanged      bool       `json:"decision_changed"`
	AddedReasons         []string   `json:"added_reasons"`
	RemovedReasons       []string   `json:"removed_reasons"`
	AddedRequiredRoles   []string   `json:"added_required_roles"`
	RemovedRequiredRoles []string   `json:"removed_required_roles"`
	Changed              bool       `json:"changed"`
}

type ShadowReport struct {
	Mode          string             `json:"mode"`
	Version       string             `json:"version"`
	ObservedAt    time.Time          `json:"observed_at"`
	CurrentHash   string             `json:"current_hash"`
	CandidateHash string             `json:"candidate_hash"`
	ChangedCases  int                `json:"changed_cases"`
	Cases         []ShadowComparison `json:"cases"`
}

// CompareSignedBundles is offline analysis, not a policy authorization decision.
// It neither installs a Bundle nor changes an Engine, Plan, Grant or Runner.
// Both signatures and lifetimes are checked on every call. Any compile/evaluation
// failure returns no report; failed analysis must not look like "no differences".
func CompareSignedBundles(ctx context.Context, current, candidate Bundle, issuer string, keys BundleKeyResolver, cases []ShadowCase, now time.Time) (ShadowReport, error) {
	if len(cases) == 0 || len(cases) > maximumShadowCases {
		return ShadowReport{}, fmt.Errorf("SHADOW_CASES_INVALID: expected 1..%d cases", maximumShadowCases)
	}
	ids := make(map[string]bool, len(cases))
	for _, c := range cases {
		if strings.TrimSpace(c.ID) == "" || ids[c.ID] || NormalizeInput(c.Input).Version != InputVersionV1Alpha1 {
			return ShadowReport{}, fmt.Errorf("SHADOW_CASES_INVALID: empty/duplicate ID or unsupported input version")
		}
		ids[c.ID] = true
	}
	active, err := newOfflineShadowOPA(ctx, current, issuer, keys, now)
	if err != nil {
		return ShadowReport{}, fmt.Errorf("SHADOW_CURRENT_INVALID: %w", err)
	}
	proposed, err := newOfflineShadowOPA(ctx, candidate, issuer, keys, now)
	if err != nil {
		return ShadowReport{}, fmt.Errorf("SHADOW_CANDIDATE_INVALID: %w", err)
	}
	report := ShadowReport{Mode: ShadowReportMode, Version: ShadowReportVersion, ObservedAt: now.UTC(), CurrentHash: current.Hash, CandidateHash: candidate.Hash, Cases: make([]ShadowComparison, 0, len(cases))}
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			return ShadowReport{}, err
		}
		input := NormalizeInput(c.Input)
		hash, err := InputHash(input)
		if err != nil {
			return ShadowReport{}, err
		}
		before, err := active.Evaluate(ctx, input)
		if err != nil {
			return ShadowReport{}, fmt.Errorf("SHADOW_CURRENT_EVALUATION: %w", err)
		}
		after, err := proposed.Evaluate(ctx, input)
		if err != nil {
			return ShadowReport{}, fmt.Errorf("SHADOW_CANDIDATE_EVALUATION: %w", err)
		}
		if before.InputHash != hash || after.InputHash != hash || before.PolicyHash != current.Hash || after.PolicyHash != candidate.Hash {
			return ShadowReport{}, fmt.Errorf("SHADOW_BINDING_MISMATCH")
		}
		compared := ShadowComparison{CaseID: c.ID, InputHash: hash, Current: before, Candidate: after, DecisionChanged: before.Decision != after.Decision, AddedReasons: shadowDifference(after.Reasons, before.Reasons), RemovedReasons: shadowDifference(before.Reasons, after.Reasons), AddedRequiredRoles: shadowDifference(after.RequiredRoles, before.RequiredRoles), RemovedRequiredRoles: shadowDifference(before.RequiredRoles, after.RequiredRoles)}
		compared.Changed = compared.DecisionChanged || len(compared.AddedReasons) > 0 || len(compared.RemovedReasons) > 0 || len(compared.AddedRequiredRoles) > 0 || len(compared.RemovedRequiredRoles) > 0
		if compared.Changed {
			report.ChangedCases++
		}
		report.Cases = append(report.Cases, compared)
	}
	return report, nil
}

// A candidate is evaluated without network, environment/runtime introspection,
// random or wall-clock builtins. This restriction applies only to this new
// analysis path and does not alter the existing enforcement engine.
func newOfflineShadowOPA(ctx context.Context, bundle Bundle, issuer string, keys BundleKeyResolver, now time.Time) (*OPA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := VerifyBundle(ctx, bundle, issuer, keys, now); err != nil {
		return nil, err
	}
	capabilities := ast.CapabilitiesForThisVersion()
	allowed := make([]*ast.Builtin, 0, len(capabilities.Builtins))
	for _, builtin := range capabilities.Builtins {
		if !builtin.IsNondeterministic() && builtin.Name != "opa.runtime" {
			allowed = append(allowed, builtin)
		}
	}
	capabilities.Builtins = allowed
	capabilities.AllowNet = []string{}
	options := []func(*rego.Rego){rego.Query("data.themisy.authorization.decision"), rego.Capabilities(capabilities)}
	for _, module := range normalizedBundle(bundle).Modules {
		options = append(options, rego.Module(string(module.Layer)+"/"+module.Name, module.Source))
	}
	query, err := rego.New(options...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile offline shadow bundle: %w", err)
	}
	return &OPA{query: query, bundleVersion: bundle.Version, bundleHash: bundle.Hash}, nil
}

func shadowDifference(left, right []string) []string {
	excluded := make(map[string]bool, len(right))
	for _, value := range right {
		excluded[value] = true
	}
	result := []string{}
	for _, value := range left {
		if !excluded[value] {
			result = append(result, value)
			excluded[value] = true
		}
	}
	sort.Strings(result)
	return result
}
