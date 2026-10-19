# ADR-0011: Opt-in OIDC principals for existing API identity hooks

- Status: Proposed
- Date: 2026-10-20
- Related: Issue #12, ADR-0006

## Context

The API exposes an `IdentityResolver` hook for approval and control operations.
OIDC verification and refreshing JWKS resolution exist, but there is no concrete
OIDC adapter from verified claims to this hook. Header-based development identity
is insufficient as evidence of IdP membership. A complete production API also
requires authentication and authorization beyond the existing identity hooks.

## Proposed decision

Add an opt-in `OIDCIdentityResolver` for one operator-configured tenant, issuer
and audience, with explicit tenant/role claim names and exact membership-to-role
mapping. It calls the actual OIDC verifier on every request. Only a verified
signed tenant claim matching this instance and a strict array of signed group
strings can produce a Principal. Unknown memberships grant no roles. Missing or
malformed claims fail closed. Role strings are operator-owned mappings, never
copied verbatim from a request. Configuration collections are copied at creation.

Bearer authentication accepts one bounded Authorization header. Actor/role
headers and form/body fields are not read by this resolver. An optional tenant
routing header must agree with the verified configured tenant; duplicate hints
fail rather than selecting one. Resolved Principals contain subject ID, tenant
and sorted unique roles only, never tokens or raw claims. Invalid identity uses
a stable generic error without propagating token or resolver error contents.

This resolver can be explicitly supplied to `NewWithIdentity`; no command,
deployment default, configuration setting or public API contract is changed.
It applies only to routes already using the hook. The service remains a
prototype; this is not whole-API production authentication.

## Alternatives

- Trust caller role/actor headers: retained solely as the existing explicit
  development path, not consumed by the new resolver.
- Copy all token roles directly: rejected in favor of explicit tenant-scoped
  mappings and claim schema.
- Live organization Directory lookup: useful for immediate role removal, but
  requires separate availability, freshness, credentials and audit design. It is
  not emulated with a cached membership success.

## Consequences and limits

The IdP must ensure tenant and group claims are authoritative and cannot be
edited by end users. Operators must choose issuer/audience/tenant trust
independently of incoming tokens. Each tenant gets a separate resolver and role
mapping. The configured JWKS resolver retains ownership of refresh/freshness and
key removal; this adapter adds no positive authentication cache.

A role removed from a newly issued token takes effect on the next request; an
already issued valid token can still assert its earlier membership until expiry
or actual issuer/key revocation. This does not solve immediate Directory role
revocation, Delegation issuance/revocation, full approval proof persistence,
Grant-time identity revalidation, cross-issuer subject namespacing, whole-API
tenant authorization or identity decision auditing. Existing approval application
checks still enforce required roles and requester/approver separation within
this single-issuer tenant. Enabling a production deployment requires all those
remaining Issue #12 conditions, a reviewed integration design and maintainer
acceptance; proposed implementation is not authorization to deploy.

## Validation

Signed synthetic Ed25519 JWTs pass through the real verifier and API approval
handler. Tests cover exact mapping, immutable config, role removal on reissued
tokens, actor/header/body escalation refusal, issuer/audience/tenant/signature/
key/expiry/claim-shape failures, duplicate headers, cancellation, removed keys,
required role refusal and self-approval refusal. Application storage and workflow
signals are fakes in these API scenarios, not real IdP/Directory or live Writes.
An additional local TLS Discovery/JWKS scenario crosses cache expiry and refuses
an approval when key refresh is unavailable or all keys have been removed.
