package identity

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"themisy/pkg/protocol"
)

var ErrDelegationDenied = errors.New("AGENT_DELEGATION_DENIED")

type Delegation struct {
	ID               string
	Issuer           string
	UserSubject      string
	AgentID          string
	Actions          []protocol.Capability
	ServiceSelectors []string
	Environments     []string
	MaximumRisk      string
	ExpiresAt        time.Time
}

type DelegationResolver interface {
	ResolveDelegation(context.Context, string) (Delegation, error)
}

type StaticDelegations map[string]Delegation

func (s StaticDelegations) ResolveDelegation(ctx context.Context, reference string) (Delegation, error) {
	if ctx.Err() != nil {
		return Delegation{}, ErrDelegationDenied
	}
	delegation, ok := s[reference]
	if !ok {
		return Delegation{}, fmt.Errorf("delegation not found")
	}
	return copyDelegation(delegation), nil
}

type DelegationRequest struct {
	Reference   string
	Subject     Subject
	AgentID     string
	Capability  protocol.Capability
	Service     string
	Environment string
	Risk        string
}

type DelegationVerifier struct {
	Resolver DelegationResolver
	Now      func() time.Time
}

func (v *DelegationVerifier) Verify(ctx context.Context, request DelegationRequest) (Delegation, error) {
	if v == nil || v.Resolver == nil || request.Reference == "" || ctx.Err() != nil || request.Subject.Issuer == "" || request.Subject.ID == "" {
		return Delegation{}, ErrDelegationDenied
	}
	delegation, err := v.Resolver.ResolveDelegation(ctx, request.Reference)
	if err != nil {
		return Delegation{}, ErrDelegationDenied
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	if ctx.Err() != nil || now.IsZero() || !request.Subject.ExpiresAt.After(now) || (!request.Subject.IssuedAt.IsZero() && request.Subject.IssuedAt.After(now)) {
		return Delegation{}, ErrDelegationDenied
	}
	if delegation.ID != request.Reference || delegation.UserSubject != request.Subject.ID || delegation.Issuer != request.Subject.Issuer || delegation.AgentID != request.AgentID || !delegation.ExpiresAt.After(now) {
		return Delegation{}, ErrDelegationDenied
	}
	requestedRisk, maximumRisk := riskLevel(request.Risk), riskLevel(delegation.MaximumRisk)
	if requestedRisk == 0 || maximumRisk == 0 || !capabilityAllowed(delegation.Actions, request.Capability) || !stringAllowed(delegation.Environments, request.Environment) || !serviceAllowed(delegation.ServiceSelectors, request.Service) || requestedRisk > maximumRisk {
		return Delegation{}, ErrDelegationDenied
	}
	return copyDelegation(delegation), nil
}

func copyDelegation(d Delegation) Delegation {
	d.Actions = slices.Clone(d.Actions)
	d.ServiceSelectors = slices.Clone(d.ServiceSelectors)
	d.Environments = slices.Clone(d.Environments)
	return d
}

func capabilityAllowed(values []protocol.Capability, expected protocol.Capability) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func stringAllowed(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func serviceAllowed(selectors []string, service string) bool {
	for _, selector := range selectors {
		matched, err := path.Match(selector, service)
		if err == nil && matched {
			return true
		}
	}
	return false
}
func riskLevel(risk string) int {
	switch risk {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	default:
		return 0
	}
}
