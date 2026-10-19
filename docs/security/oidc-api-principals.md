# Opt-in OIDC identity for approval and control hooks

`api.NewOIDCIdentityResolver` creates an `IdentityResolver` for the existing
`api.NewWithIdentity` constructor. It is not enabled by `cmd/themisy` and does not
authenticate routes that do not already use this hook. Do not deploy this unit
as complete production API authentication. See proposed
[ADR-0011](../adr/0011-oidc-api-principal-resolution.md).

The operator supplies one tenant ID, trusted OIDC verifier (issuer/audience/key
resolver), the IdP's authoritative tenant and group claim names, and exact group
to application-role mappings. For example, a verified group value `ops-team`
may map to `sre`; a signed token containing `sre` does not itself receive that
role unless `sre` was also configured as a mapped group. Group claims must be
arrays of strings. Unknown memberships confer no roles. Tenant and group claims
must be controlled by the IdP/organization administrator, not user-editable
profile fields. Supply per-tenant trust independently of request content.

Each operation provides exactly one `Authorization: Bearer ...` header. Identity
is verified anew using the configured OIDC/JWKS resolver; no successful Principal
is cached here. Optional `X-Tenant-ID` is checked for agreement and cannot choose
another tenant. `X-Actor-ID`, `X-Actor-Roles` and form/body role fields are ignored
by this resolver. Missing/expired/invalid identity or malformed membership fails
with `UNAUTHENTICATED_PRINCIPAL` and no Principal. Error contents do not include
tokens. Only subject ID, tenant ID and mapped roles reach application checks.

The existing approval application still checks the required role and rejects
the requester approving their own request. Tests use signed synthetic JWTs
through the actual approval handler, with in-memory application storage and fake
workflow signals. Invalid identity, header escalation, missing roles and
self-approval send no approval signal. Removed trust keys are refused on later
requests. These are not live IdP/Directory or credential/Adapter Write tests.

A composed scenario also serves Discovery/JWKS over a local TLS test server,
first authenticates successfully, advances the configured test clock past the
key-cache TTL, then makes JWKS unavailable or removes all keys. The same valid
JWT cannot send an approval signal afterward. This exercises real HTTPS key
refresh and the approval handler together, with synthetic IdP data; it does not
prove an external provider's availability, user-revocation semantics or live
Write behavior.

A newly issued token without a membership immediately resolves without its
role. Existing valid tokens retain signed memberships until expiry/revocation;
there is no live Directory lookup or immediate per-user revocation in this unit.
JWKS cache/freshness remains governed by the separately configured
[resolver](oidc-jwks-resolver.md). Role/Delegation revocation at Grant issuance,
identity audit, all-route authentication, real IdP rollout, issuer-qualified
subject persistence and trusted-proxy edge configuration remain open. Do not
make the Header development resolver a fallback after OIDC failure.

Validation: `go test -race ./internal/api ./internal/identity`. Independent
maintainer review, proposed ADR acceptance and full Issue #12 acceptance remain
outstanding.
