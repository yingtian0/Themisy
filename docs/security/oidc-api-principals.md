# Opt-in OIDC identity for approval and control hooks

`api.NewOIDCIdentityResolver` creates an `IdentityResolver` for the existing
`api.NewWithIdentity` constructor. It is not enabled by `cmd/themisy` and does not
authenticate routes that do not already use this hook. Do not deploy this unit
as complete production API authentication. See proposed
[ADR-0014](../adr/0014-oidc-api-principal-resolution.md).

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
all-route authentication, real IdP rollout, issuer-qualified
subject persistence and trusted-proxy edge configuration remain open. Do not
make the Header development resolver a fallback after OIDC failure.

Validation: `go test -race ./internal/api ./internal/identity`. Independent
maintainer review, proposed ADR acceptance and full Issue #12 acceptance remain
outstanding.

## Audit admission gate

`NewAuditedOIDCIdentityResolver(config, sink)` is an additional explicit choice
for the same identity hook. Its sink must implement
`AppendAuditContext(context.Context, domain.AuditEvent) error`, commit before
returning nil and honor cancellation. The existing PostgreSQL Store implements
this method transactionally; Memory is only a test/development sink. A missing
sink or any append failure returns `IDENTITY_AUDIT_UNAVAILABLE` with no Principal. A valid
identity is not admitted on the strength of an uncommitted audit or a previous
decision. The default non-audited constructor is unchanged; operators must not
substitute it as a fallback after failure of the audit gate.

Each call appends one `identity.resolve` event under a server-generated
`identity/<configured-tenant>/<random-id>` correlation. It includes configured
tenant/issuer/audience and stable reason codes `OIDC_PRINCIPAL_VERIFIED` or
`OIDC_PRINCIPAL_REJECTED`. Only successful verification adds subject, mapped
roles and proof expiry. Invalid identity records no unverified subject or claim.
Authorization headers, bearer tokens, arbitrary claims, body/delegation fields,
request URLs, query values and caller correlation IDs are not copied. A
Delegation reference is intentionally absent until a trusted Delegation
verification path exists. Audit access remains operator-side through the
existing store; this adds no public audit endpoint.

Audit writes have a five-second ceiling bounded by the request context. There
is no internal append retry or read-back-to-allow fallback on ambiguous storage
responses. Even if an event committed but its response was lost, no Principal is
returned. Cancellation or proof expiry during a slow append also refuses
admission. An event describes verification at its timestamp, not business-action
authorization or approval success; application operations retain their own
transactional audit. Principals carry normalized issuer and expiry metadata,
without the original token. Existing authentication cache/role-revocation limits
above still apply.

`make test-identity-audit` creates an owned PostgreSQL 17.6 Alpine tmpfs container
with synthetic credentials and a dynamic loopback port, migrates only a fresh
specifically named fixture database, runs the integration test with race, and
removes that exact container and its anonymous volumes. It does not use caller
database URLs, existing Compose services, customer data or destructive resets.
Cleanup errors fail the target. The direct tagged test skips if its dedicated
endpoint is absent; that skip is not a live database verification result.

The real PostgreSQL test checks VERIFIED/REJECTED events across store reopen and
uses a separate read-only client session to make audit INSERT fail, then proves
zero approval workflow signals. Unit tests also cover commit-response loss,
secret canaries, cancellation and expiry during append. The PostgreSQL persistence
is real; the IdP JWTs are synthetic and application/workflow fixtures are still
memory/fakes. This unit does not complete identity-decision correlation to
Plan/Grant/Delegation or the whole Issue #12 audit/production acceptance gate.

## Delegation lookup lifetime

Delegation verification now checks the already-verified Subject again after
resolving the trusted delegation record. A missing/expired Subject expiry, future
Subject issuance, cancellation before/during lookup or an expired delegation
returns the stable AGENT_DELEGATION_DENIED result without a usable record.
Raw resolver diagnostics are not propagated. Both the static resolver and final
verifier return independently owned action, service-selector and environment
slices, so modifying returned scopes cannot change later authorization.

Callers constructing Subjects directly must provide the real verified expiry;
a zero expiry is no longer accepted as an indefinitely valid proof. This does
not verify an arbitrary caller-created Subject or replace the OIDC verifier.
Existing Runner composition supplies the verified Subject. No signed Delegation,
Directory revocation protocol, tenant model or command default is introduced;
those remaining production trust-chain requirements stay open under Issue #12.
Tests cover expiry during lookup, cancellation, diagnostics and scope-copy isolation.
