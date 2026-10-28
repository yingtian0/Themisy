package identity

import (
	"context"
	"fmt"
	"slices"
	"themisy/internal/domain"
)

func BindIntentActor(ctx context.Context, intent *domain.ReleaseIntent) error {
	actor, ok := RequestActorFromContext(ctx)
	if !ok {
		if HasRequestActor(ctx) {
			return ErrInvalidIdentityProof
		}
		return nil
	}
	if (intent.TenantID != "" && intent.TenantID != actor.TenantID) || (intent.RequestedBy != "" && intent.RequestedBy != actor.Subject) || (intent.SubjectIssuer != "" && intent.SubjectIssuer != actor.Issuer) || (intent.SubjectType != "" && intent.SubjectType != actor.SubjectType) || (intent.UserIdentityProof != "" && intent.UserIdentityProof != actor.ProofToken) {
		return fmt.Errorf("authenticated requester binding mismatch")
	}
	if !slices.Contains(actor.Roles, "deployer") {
		return fmt.Errorf("deployer role required")
	}
	intent.TenantID = actor.TenantID
	intent.RequestedBy = actor.Subject
	intent.SubjectIssuer = actor.Issuer
	intent.SubjectType = actor.SubjectType
	intent.UserIdentityProof = actor.ProofToken
	intent.Agent.Scopes = []string{"release:deploy"}
	return nil
}
