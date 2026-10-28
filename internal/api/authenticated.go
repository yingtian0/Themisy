package api

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"themisy/internal/domain"
	"themisy/internal/identity"
	"time"
)

type ProductionIdentityResolver struct {
	OIDC      *AuditedOIDCIdentityResolver
	Directory *identity.FileDirectory
	Audit     IdentityAuditSink
	Now       func() time.Time
}

func (p *ProductionIdentityResolver) Resolve(r *http.Request) (Principal, error) {
	if p == nil || p.OIDC == nil || p.Directory == nil || p.Audit == nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	principal, err := p.OIDC.Resolve(r)
	if err != nil || principal.TenantID != p.Directory.TenantID {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	member, directoryErr := p.Directory.Member(r.Context(), principal.Issuer, principal.Subject)
	roles := []string{}
	for _, role := range principal.Roles {
		if slices.Contains(member.Roles, role) {
			roles = append(roles, role)
		}
	}
	principal.Roles = roles
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	result, reason := "VERIFIED", "DIRECTORY_PRINCIPAL_VERIFIED"
	if directoryErr != nil {
		result, reason = "REJECTED", "DIRECTORY_PRINCIPAL_UNAVAILABLE"
	}
	event := domain.AuditEvent{ID: "directory_decision_" + rand.Text(), CorrelationID: "identity/" + principal.TenantID, ActorType: "user", ActorID: principal.Subject, Action: "identity.directory", ResourceType: "identity_decision", ResourceID: principal.Subject, Result: result, Timestamp: now, Details: map[string]any{"tenant_id": principal.TenantID, "issuer": principal.Issuer, "roles": slices.Clone(roles), "reason_code": reason}}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if p.Audit.AppendAuditContext(ctx, event) != nil || ctx.Err() != nil || !principal.ExpiresAt.After(p.now()) {
		return Principal{}, ErrIdentityAuditUnavailable
	}
	if directoryErr != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	latest, err := p.Directory.Member(r.Context(), principal.Issuer, principal.Subject)
	if err != nil || !slices.Equal(latest.Roles, member.Roles) || !principal.ExpiresAt.After(p.now()) {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	return principal, nil
}
func (p *ProductionIdentityResolver) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Authenticate protects every route of the wrapped surface. Runner transport
// and health use separate handlers; no header fallback is installed here.
func Authenticate(next http.Handler, resolver IdentityResolver, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if resolver == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "IDENTITY_UNAVAILABLE"})
			return
		}
		principal, err := resolver.Resolve(r)
		if err != nil || !principal.Verified || principal.IdentityProof == "" {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "IDENTITY_UNAVAILABLE"})
			return
		}
		clock := now
		if clock == nil {
			clock = time.Now
		}
		lifetime := principal.ExpiresAt.Sub(clock().UTC())
		if lifetime <= 0 {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "IDENTITY_UNAVAILABLE"})
			return
		}
		bounded, cancel := context.WithTimeout(r.Context(), lifetime)
		defer cancel()
		actor := identity.RequestActor{TenantID: principal.TenantID, Subject: principal.Subject, Issuer: principal.Issuer, SubjectType: principal.SubjectType, Roles: principal.Roles, ProofToken: principal.IdentityProof, ExpiresAt: principal.ExpiresAt}
		next.ServeHTTP(w, r.WithContext(identity.WithRequestActor(bounded, actor, now)))
	})
}

type ContextIdentityResolver struct{}

func (ContextIdentityResolver) Resolve(r *http.Request) (Principal, error) {
	a, ok := identity.RequestActorFromContext(r.Context())
	if !ok {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	return Principal{Subject: a.Subject, TenantID: a.TenantID, Issuer: a.Issuer, SubjectType: a.SubjectType, Roles: a.Roles, IdentityProof: a.ProofToken, ExpiresAt: a.ExpiresAt, Verified: true}, nil
}
func operatorAllowed(p Principal) bool {
	if !p.Verified {
		return true
	}
	return slices.Contains(p.Roles, "operator") || slices.Contains(p.Roles, "admin") || slices.Contains(p.Roles, "sre")
}
