package main

import (
	"context"
	"fmt"
	"net/http"
	"themisy/internal/api"
	"themisy/internal/application"
	"themisy/internal/config"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/mcp"
	postgresstore "themisy/internal/store/postgres"
	"themisy/internal/ui"
	"time"
)

func buildIdentity(ctx context.Context, c config.Config, st *postgresstore.Store, grants *application.Grants) (*application.IdentityAuthority, api.IdentityResolver, error) {
	if err := c.Identity.Validate(); err != nil {
		return nil, nil, err
	}
	if c.Identity.Mode == "development-headers" {
		return nil, api.HeaderIdentityResolver{}, nil
	}
	cfg := c.Identity
	ttl, _ := time.ParseDuration(cfg.CacheTTL)
	keys, err := identity.NewOIDCJWKSResolver(map[string]identity.OIDCIssuerConfig{cfg.Issuer: {DiscoveryURL: cfg.DiscoveryURL, CacheTTL: ttl}})
	if err != nil {
		return nil, nil, err
	}
	oidc := identity.OIDCVerifier{Issuer: cfg.Issuer, Audience: cfg.Audience, Keys: keys}
	directoryKeys := grant.FileKeys{Path: cfg.DirectoryTrustBundleFile, Issuer: cfg.DirectoryIssuer, Tenant: cfg.TenantID}
	directory := &identity.FileDirectory{Path: cfg.DirectoryFile, Issuer: cfg.DirectoryIssuer, TenantID: cfg.TenantID, MinimumRevision: cfg.DirectoryMinimumRevision, Keys: directoryKeys, Ledger: st}
	if _, err := directory.Read(ctx); err != nil {
		return nil, nil, fmt.Errorf("identity Directory startup: %w", err)
	}
	attestationKeys := grant.FileKeys{Path: cfg.AttestationTrustBundleFile, Issuer: c.Grants.Issuer, Tenant: cfg.TenantID}
	if err := attestationKeys.Validate(ctx); err != nil {
		return nil, nil, err
	}
	authority := &application.IdentityAuthority{Audit: st, Signer: grants.Signer, KeyID: c.Grants.Signing.KeyID, Verifier: &identity.AttestationVerifier{Issuer: c.Grants.Issuer, TenantID: cfg.TenantID, Audience: cfg.Audience, Keys: attestationKeys, OIDC: &oidc, Directory: directory}}
	audited, err := api.NewAuditedOIDCIdentityResolver(api.OIDCPrincipalConfig{TenantID: cfg.TenantID, TenantClaim: cfg.TenantClaim, RoleClaim: cfg.RoleClaim, SubjectTypeClaim: cfg.SubjectTypeClaim, RoleMappings: cfg.RoleMappings, Verifier: oidc}, st)
	if err != nil {
		return nil, nil, err
	}
	return authority, &api.ProductionIdentityResolver{OIDC: audited, Directory: directory, Audit: st}, nil
}

type contextUIIdentity struct{}

func (contextUIIdentity) Verify(r *http.Request) (ui.Identity, error) {
	a, ok := identity.RequestActorFromContext(r.Context())
	if !ok {
		return ui.Identity{}, api.ErrUnauthenticatedPrincipal
	}
	return ui.Identity{Subject: a.Subject, TenantID: a.TenantID, Roles: a.Roles}, nil
}

type contextMCPIdentity struct{}

func (contextMCPIdentity) Resolve(r *http.Request) (mcp.Principal, error) {
	a, ok := identity.RequestActorFromContext(r.Context())
	if !ok {
		return mcp.Principal{}, api.ErrUnauthenticatedPrincipal
	}
	return mcp.Principal{Subject: a.Subject, TenantID: a.TenantID, Roles: a.Roles}, nil
}
