package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"themisy/internal/identity"
)

var ErrUnauthenticatedPrincipal = errors.New("UNAUTHENTICATED_PRINCIPAL")

// OIDCPrincipalConfig belongs to one trusted tenant/issuer/audience tuple. Claim
// names and role mappings are operator configuration, never request parameters.
type OIDCPrincipalConfig struct {
	TenantID         string
	SubjectTypeClaim string
	TenantClaim      string
	RoleClaim        string
	RoleMappings     map[string][]string
	Verifier         identity.OIDCVerifier
}

// OIDCIdentityResolver is an opt-in resolver for routes already using
// IdentityResolver. It is not authentication middleware for all API endpoints.
type OIDCIdentityResolver struct {
	tenantID         string
	subjectTypeClaim string
	tenantClaim      string
	roleClaim        string
	roles            map[string][]string
	verifier         identity.OIDCVerifier
}

func NewOIDCIdentityResolver(config OIDCPrincipalConfig) (*OIDCIdentityResolver, error) {
	if strings.TrimSpace(config.TenantID) != config.TenantID || config.TenantID == "" ||
		config.TenantClaim == "" || config.RoleClaim == "" || config.TenantClaim == config.RoleClaim ||
		config.Verifier.Issuer == "" || config.Verifier.Audience == "" || config.Verifier.Keys == nil {
		return nil, ErrUnauthenticatedPrincipal
	}
	roles := make(map[string][]string, len(config.RoleMappings))
	for group, mapped := range config.RoleMappings {
		if group == "" || len(mapped) == 0 {
			return nil, ErrUnauthenticatedPrincipal
		}
		for _, role := range mapped {
			if role == "" || strings.TrimSpace(role) != role {
				return nil, ErrUnauthenticatedPrincipal
			}
		}
		roles[group] = append([]string(nil), mapped...)
	}
	return &OIDCIdentityResolver{tenantID: config.TenantID, subjectTypeClaim: config.SubjectTypeClaim, tenantClaim: config.TenantClaim, roleClaim: config.RoleClaim, roles: roles, verifier: config.Verifier}, nil
}

func (o *OIDCIdentityResolver) Resolve(r *http.Request) (Principal, error) {
	if o == nil || r == nil || r.Context().Err() != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || len(headers[0]) > 16384 {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	parts := strings.Fields(headers[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	subject, err := o.verifier.Verify(r.Context(), parts[1])
	if err != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	now := time.Now().UTC()
	if o.verifier.Now != nil {
		now = o.verifier.Now().UTC()
	}
	tenant, ok := subject.Claims[o.tenantClaim].(string)
	if !ok || tenant != o.tenantID || !subject.ExpiresAt.After(now) {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	// Tenant headers are optional routing hints, not identity. An explicit hint
	// must agree; duplicate hints are rejected rather than choosing one.
	tenantHeaders := r.Header.Values("X-Tenant-ID")
	if len(tenantHeaders) > 1 || (len(tenantHeaders) == 1 && tenantHeaders[0] != o.tenantID) {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	groups, ok := subject.Claims[o.roleClaim].([]any)
	if !ok {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	resolved := map[string]bool{}
	for _, group := range groups {
		name, ok := group.(string)
		if !ok || name == "" {
			return Principal{}, ErrUnauthenticatedPrincipal
		}
		for _, role := range o.roles[name] {
			resolved[role] = true
		}
	}
	if r.Context().Err() != nil {
		return Principal{}, ErrUnauthenticatedPrincipal
	}
	roles := make([]string, 0, len(resolved))
	for role := range resolved {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	subjectType := "user"
	if o.subjectTypeClaim != "" {
		if value, exists := subject.Claims[o.subjectTypeClaim]; exists {
			var ok bool
			subjectType, ok = value.(string)
			if !ok || (subjectType != "user" && subjectType != "ci") {
				return Principal{}, ErrUnauthenticatedPrincipal
			}
		}
	}
	return Principal{Verified: true, IdentityProof: parts[1], SubjectType: subjectType, Subject: subject.ID, TenantID: o.tenantID, Roles: roles, Issuer: subject.Issuer, ExpiresAt: subject.ExpiresAt}, nil
}
