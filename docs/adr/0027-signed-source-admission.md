# ADR 0027: Customer-collected signed pre-dispatch source admission

- Status: Proposed
- Date: 2026-10-28
- Scope: #13; live staging remains an external acceptance gate

## Problem

Typed CI/dependency/metadata collectors and audited observations exist but are
not the standard Planner/Grant authority. Plain observation records are not
trusted values. A GitHub artifact archive digest also is not container image
provenance and must not be treated as one.

## Decision and trust boundary

A customer-side read-only collector uses the existing authoritative adapters,
operator allowlists and local provider credentials. It publishes a short-lived,
signed per-tenant admission envelope containing explicit targets, with a global
monotonic revision for that tenant/source issuer. Provision
that protected file to Control Plane and Runner; neither receives provider
credentials. Source signing, CI build provenance signing and Grant signing use
independent issuers/trust. Signed negative collection results advance the source
fence and cannot be replaced with an older healthy envelope.

Require an independently signed build statement binding repository, commit,
workflow/run/attempt and actual deployable artifact digest. Validate it against
the collected successful CI run. Archive digests remain separate source
identifiers. Signing permission belongs to the trusted build workload, not an
Agent, Control Plane or context reader. This is a deliberately narrow Themisy
build statement, not a claim of complete SLSA/Sigstore verification.

Import signed envelopes only through signature, scope, canonical content hash,
clock/freshness, source/provenance and validated metadata checks. Do not add JSON
deserialization into the existing opaque observation types. Independently
validate Contract/Profile bytes and the trusted repository/revision/path
allowlist. Derive CI and dependency flags from PASS observations, never from
request booleans. Retain FAIL/INCONCLUSIVE/MISSING as explicit refusal.

Plan the trusted Contract/Profile set and pin the aggregate source hash, artifact
binding and the envelope revision/expiry into the immutable Plan. Store
sanitized source records for reproducibility. New Grants and Runner writes must
re-read current signed files, trust, revision fences and expiry and match that
exact pinned context. Any change requires a new plan/approval; renewal cannot
silently extend an old snapshot. Cap Grant expiry to the source freshness bound.

## Consequences

The standard OIDC execution mode requires signed source admission; development
synthetic inputs remain explicit. Collection/provisioning is customer-owned and
must publish negative results atomically on provider failure. A snapshot is
usable only within operator-configured maximum ages; it does not claim that an
unobserved provider change after capture is immediately known. No previous PASS
is reused by a failed collection. No source object is permission for an unrelated
Write, and missing files or failed trust/audit/fences stop new authorization.

Separate customer/runtime DB fences survive restart. New databases/restores need
an independently trusted minimum revision; backups alone do not establish the
latest source state. Existing source-observation audit objects are not upgraded
into authority. Existing Runs/approvals are not rewritten. Production fleet and
schema migration must quiesce writes and replan/reapprove.

## Validation

Use real read-only adapter implementations against provider HTTP fixtures, not
caller-supplied success fields. Cover wrong repository/commit/artifact/run,
expired/changed sources, non-PASS dependencies, untrusted metadata, provider
failure and revoked/tampered signatures with zero new Grant/credential/Write
calls. Test deterministic hashes and independent PostgreSQL restart fences.
Real CI and runtime/observability staging with customer provisioned credentials,
artifact statements and evidence is still required before closing that gate;
do not substitute fixtures for external evidence.
