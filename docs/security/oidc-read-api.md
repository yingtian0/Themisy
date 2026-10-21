# Tenant-bound OIDC summary read API

`api.NewOIDCReadAPI(model, config, sink)` creates a separate opt-in HTTP handler.
The model implements only tenant-aware reads. Set one trusted
`OIDCReadConfig.Identity` and a nonempty `RequiredRole` that your OIDC group
mapping grants to readers. Supply a durable `AppendAuditContext` sink, such as
the PostgreSQL store. Memory is for local tests only. The constructor builds the
audited OIDC resolver itself; it accepts no Header identity resolver.

Every request except exact GET/HEAD `/healthz` verifies the bearer proof and
commits the identity decision before routing. A mapped read role is required.
Model calls use only the configured tenant that matched the signed tenant claim.
Omitting `X-Tenant-ID` does not select `default`. Explicit duplicate/mismatched
tenant headers refuse authentication; body/query tenant fields never select a
read scope. Role and actor headers grant no authority. Identity failures return
401, missing mapped role 403, non-read methods 405. All responses use no-store.

## Summary routes

| GET/HEAD route | Response |
|---|---|
| `/v1/plans/{id}` | plan ID, tenant, environment, hash, creation/expiry times |
| `/v1/release-runs/{id}` | Run ID, tenant, environment/status, Plan hash, state version, step service/status, timestamps |
| `/v1/release-runs/{id}/events` | event ID, action, resource type/ID, result, timestamp |
| `/v1/approvals` | tenant-local pending approval summaries |
| `/v1/runners` | tenant-local Runner projections |

The Plan/Run/event response contract intentionally uses summaries rather than
the development API's complete domain serialization. Proofs, Delegation refs,
free-form failure fields, actor identity metadata and arbitrary event Details
are absent. Existing readiness/status values are retained; the reader does not
turn UNKNOWN, MISSING or unverified state into a success claim. Cross-tenant,
missing and storage-failed record lookups share 404 `OIDC_READ_UNAVAILABLE`.
An authentication VERIFIED audit is not evidence of a successful business read.

No planning, release start, approval decision, Runner control or contract
validation endpoint is registered, including deprecated command aliases. The
global service list is absent because its interface has no tenant parameter.
Do not add a fallback to `api.New` or `NewWithIdentity` on the same listener.

## Composition and validation

This handler is not enabled by existing CLI/server defaults and does not change
their OpenAPI contract. A host must explicitly adopt the smaller summary
protocol, provide HTTPS and bounded HTTP server timeouts, configure one trusted
issuer/tenant/audience and durable audit, and use the existing JWKS resolver
instructions. `/healthz` reports process liveness, not IdP/store readiness.
Do not expose the development command listener as part of this composition.

Run `go test -race ./internal/api ./internal/identity` and
`make test-oidc-read-api`. The latter owns a fresh dedicated PostgreSQL fixture,
ignores caller database settings, binds a dynamic loopback port and cleans only
its own container/volumes. It verifies tenant reads across store reopen and zero
model reads when the server refuses audit INSERT. The HTTP/token/role fixtures
remain synthetic, including the local TLS IdP. No live Directory or production
Write is exercised.

Write Identity/proof binding, signed revocable Delegation, Grant-time
revalidation, issuer-qualified subjects, business-operation audit correlation,
immediate role revocation and live IdP/staging acceptance remain #12 work. See
[proposed ADR16](../adr/0016-tenant-bound-oidc-read-api.md).
