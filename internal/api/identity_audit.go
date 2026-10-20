package api

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"time"

	"themisy/internal/domain"
)

var ErrIdentityAuditUnavailable = errors.New("IDENTITY_AUDIT_UNAVAILABLE")

// IdentityAuditSink must commit the event before returning nil and respect ctx.
// A memory sink is suitable for tests only; production must supply durable audit.
type IdentityAuditSink interface {
	AppendAuditContext(context.Context, domain.AuditEvent) error
}

// AuditedOIDCIdentityResolver gates the existing identity hook on a committed
// identity decision. It does not replace the business operation's own audit.
type AuditedOIDCIdentityResolver struct {
	resolver *OIDCIdentityResolver
	audit    IdentityAuditSink
}

func NewAuditedOIDCIdentityResolver(config OIDCPrincipalConfig, sink IdentityAuditSink) (*AuditedOIDCIdentityResolver, error) {
	if sink == nil {
		return nil, ErrIdentityAuditUnavailable
	}
	resolver, err := NewOIDCIdentityResolver(config)
	if err != nil {
		return nil, err
	}
	return &AuditedOIDCIdentityResolver{resolver: resolver, audit: sink}, nil
}

func (o *AuditedOIDCIdentityResolver) Resolve(r *http.Request) (Principal, error) {
	if o == nil || o.resolver == nil || o.audit == nil || r == nil {
		return Principal{}, ErrIdentityAuditUnavailable
	}
	principal, verificationErr := o.resolver.Resolve(r)
	now := time.Now().UTC()
	if o.resolver.verifier.Now != nil {
		now = o.resolver.verifier.Now().UTC()
	}
	if verificationErr == nil && !principal.ExpiresAt.After(now) {
		verificationErr = ErrUnauthenticatedPrincipal
		principal = Principal{}
	}
	id := "identity_" + rand.Text()
	event := domain.AuditEvent{
		ID: id, CorrelationID: "identity/" + o.resolver.tenantID + "/" + id,
		ActorType: "user", ActorID: "unverified", Action: "identity.resolve",
		ResourceType: "identity_decision", ResourceID: id, Result: "REJECTED", Timestamp: now,
		Details: map[string]any{"tenant_id": o.resolver.tenantID, "issuer": o.resolver.verifier.Issuer,
			"audience": o.resolver.verifier.Audience, "reason_code": "OIDC_PRINCIPAL_REJECTED"},
	}
	if verificationErr == nil {
		event.ActorID, event.Result = principal.Subject, "VERIFIED"
		event.Details["subject"] = principal.Subject
		event.Details["roles"] = append([]string(nil), principal.Roles...)
		event.Details["expires_at"] = principal.ExpiresAt
		event.Details["reason_code"] = "OIDC_PRINCIPAL_VERIFIED"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Never retry an ambiguous append or infer success from a previous event.
	if err := o.audit.AppendAuditContext(ctx, event); err != nil {
		return Principal{}, ErrIdentityAuditUnavailable
	}
	if ctx.Err() != nil || r.Context().Err() != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	if verificationErr != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	// Audit latency cannot extend the verified proof's lifetime.
	now = time.Now().UTC()
	if o.resolver.verifier.Now != nil {
		now = o.resolver.verifier.Now().UTC()
	}
	if !principal.ExpiresAt.After(now) {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	return principal, nil
}
