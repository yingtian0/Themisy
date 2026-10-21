# ADR-0016: Explicit tenant-bound OIDC summary read API

- Status: Proposed
- Date: 2026-10-21
- Related: Issue #12, ADR-0005, ADR-0014

## Context

The existing API identity hook can use audited OIDC authentication for selected
commands. It does not authenticate every read endpoint. Replacing the complete
development router would also expose command paths before requester/proof,
Delegation and Grant-time revalidation have a complete trust chain.

## Proposed decision

Provide a separate opt-in `NewOIDCReadAPI` constructor with a narrow read-model
interface, one operator-configured tenant/issuer/audience tuple, an explicit
required mapped role and a context-aware audit sink. Every protected request
uses the actual audited OIDC resolver before any read, without a cached
Principal or a requester-selected tenant. Header/body/query routing hints cannot
override the configured, verified tenant. Exact `/healthz` GET/HEAD is public.

Register only Plan summary, Run summary, Run event summary, pending approval
list and Runner list GET/HEAD routes. Authenticated non-read verbs return 405;
global service metadata, planning, validation, execution, approval decisions and
Runner controls have no route or command interface. All responses use no-store.

Plan/Run/events use explicit summary projections. Identity proofs, Delegation
references, free-form failure text, actor/proxy headers and arbitrary audit
Details are absent from these projections. Cross-tenant, missing and failed
record reads use the same stable unavailable response. Identity resolution
failure returns a stable response without a storage or verifier diagnostic.

An authentication event is committed before a protected read can proceed.
That VERIFIED event records identity admission only: it does not imply the
mapped read role was sufficient, the requested record existed or data was read.
There is no new business read-outcome audit or Plan/Grant correlation.

## Alternatives

- Authenticate the development router in place: rejected for this unit because
  it would couple read authentication with incomplete write identity binding.
- Accept any IdentityResolver: rejected here to keep HeaderIdentityResolver and
  unsigned synthetic Principals out of this constructor.
- Serialize complete stored Run/audit payloads: rejected in favor of a small,
  explicit summary protocol that does not automatically expose future fields.
- Mount the existing command router as a fallback: rejected; unsupported paths
  must not acquire execution authority through another handler.

## Consequences and limits

The development API, CLI, existing OpenAPI contract and command defaults are
unchanged. This is a separately composed in-process handler, not a command-line
switch, deployed listener or production readiness claim. Hosts must supply a
durable sink, HTTPS, bounded server timeouts and their trusted OIDC configuration.
Do not mount it on the development command router or substitute it for that API
without adopting its smaller response contract.

Access is tenant-wide for the configured mapped role, not per-resource ownership
or a Directory revocation check. JWKS lifetime/rotation behavior remains ADR14's;
there is no immediate group-membership revocation guarantee. Requester/proof
binding for writes, Delegation, Grant-time identity revalidation, issuer-qualified
subject persistence and live IdP/staging remain open. No new schema, signed
protocol or Workflow command graph is introduced. Maintainer review is required;
this ADR is Proposed.

## Validation

Actual signed JWTs exercise the audited resolver, real tenant-aware application
methods and a memory store. Negative cases cover all registered reads, mismatched
tenant hints, role Header escalation, bad/expired/wrong-audience proofs, removed
keys, append failure/response loss, cancellation and expiry during append. A
local TLS discovery/JWKS fixture succeeds once, then refuses all reads after key
cache expiry and fetch failure. Unsupported current and legacy command routes
invoke no read model or Workflow command. Summary canaries stay out of responses.
An isolated PostgreSQL target exercises reads after reopen and a server-side
read-only audit INSERT refusal. Local fixtures do not prove live IdP/Directory
or production authorization.
