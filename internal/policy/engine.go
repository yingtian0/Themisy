package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"themisy/internal/domain"
)

type Evaluation struct {
	Decision      domain.Decision `json:"decision"`
	Reasons       []string        `json:"reasons"`
	RequiredRoles []string        `json:"required_roles,omitempty"`
	InputHash     string          `json:"input_hash"`
	PolicyVersion string          `json:"policy_version"`
	PolicyHash    string          `json:"policy_hash,omitempty"`
}

type Evaluator interface {
	Evaluate(context.Context, Input) (Evaluation, error)
}

type Engine struct {
	evaluator Evaluator
	now       func() time.Time
}

func New() *Engine { return NewEngine(builtinEvaluator{}) }

func NewEngine(evaluator Evaluator) *Engine {
	return &Engine{evaluator: evaluator, now: time.Now}
}

func (e *Engine) EvaluateContext(ctx context.Context, input Input) (Evaluation, error) {
	if e == nil || e.evaluator == nil {
		return Evaluation{}, fmt.Errorf("POLICY_UNAVAILABLE")
	}
	return e.evaluator.Evaluate(ctx, NormalizeInput(input))
}

func (e *Engine) PolicyHash() string {
	if e == nil || e.evaluator == nil {
		return ""
	}
	provider, ok := e.evaluator.(interface{ PolicyHash() string })
	if !ok {
		return ""
	}
	return provider.PolicyHash()
}

// Evaluate preserves the Packet 00-02 API while routing through the same typed input.
func (e *Engine) Evaluate(input Input) domain.PolicyDecision {
	evaluation, err := e.EvaluateContext(context.Background(), input)
	createdAt := time.Now().UTC()
	if e != nil && e.now != nil {
		createdAt = e.now().UTC()
	}
	if err != nil {
		hash, _ := InputHash(input)
		return domain.PolicyDecision{Decision: domain.DecisionDeny, PolicyVersion: "unavailable", ReasonCode: "POLICY_UNAVAILABLE", ReasonDetail: err.Error(), InputHash: hash, CreatedAt: createdAt}
	}
	reason := "POLICY_ALLOW"
	if len(evaluation.Reasons) > 0 {
		reason = evaluation.Reasons[0]
	}
	return domain.PolicyDecision{Decision: evaluation.Decision, PolicyVersion: evaluation.PolicyVersion, ReasonCode: reason, ReasonDetail: strings.Join(evaluation.Reasons, ", "), RequiredRoles: append([]string(nil), evaluation.RequiredRoles...), InputHash: evaluation.InputHash, CreatedAt: createdAt}
}

func (e *Engine) PolicyHashFor(ctx context.Context, input Input) (string, error) {
	if e == nil || e.evaluator == nil {
		return "", ErrDistribution
	}
	if scoped, ok := e.evaluator.(interface {
		PolicyHashFor(context.Context, Input) (string, error)
	}); ok {
		return scoped.PolicyHashFor(ctx, input)
	}
	return e.PolicyHash(), nil
}
