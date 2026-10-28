# ADR-0011: Separate review freshness, execution and compensation lifetimes

- Status: Proposed; bounded implementation available, independent review pending
- Date: 2026-10-28
- Scope: Issue #51

## Decision and limits

Use UTC half-open intervals: `issued_at <= now < valid_until`. Deadline equality
refuses a new Write; signature clock skew does not extend v2 authority. Keep
immutable Plan freshness (default 15m) and short Grants (default 5m). Approval's
24h wait ceiling never extends Plan, Context or identity authority.

This implementation chooses explicit stopping rather than automatic renewal.
Forward validity is the minimum of Plan, Context and approval deadlines; identity
receipts may shorten Grant validity further. Late approval and later steps record
`REAUTHORIZATION_REQUIRED`, the expired object and deadline. Observation of an
already authorized operation may complete after expiry. A final observation can
finish the Run, but cannot authorize another Write.

The earlier proposal to provision compensation before source dispatch and allow
it after forward expiry is deferred. Implemented compensation is a separate,
source-bound, single-use permission **within the intersection of all current
original authority and the Contract rollback window**. There is no post-expiry
rollback grace period, automatic reapproval or successor Run creation.

## Epoch and independent manifest

Protocol `themisy.protocol/v1alpha2` and manifest
`themisy.plan-manifest/v1alpha2` sign `authorization_epoch` and
`authorization_valid_until`. The epoch hashes Run ID, Plan hash and original
creation/expiry, signed Context binding and approval decision. Equal Plan hashes
do not transfer permission to changed time bounds. The independent customer
manifest must match the Grant's epoch, lifetime and compensation binding.

V2 signs a domain-separated SHA256 digest of the complete canonical payload,
keeping the raw signing message below KMS limits even with large identity
receipts. Domains are `themisy.action-grant-signature/v2` and
`themisy.plan-manifest-signature/v2`, followed by NUL and the lowercase hex digest.
V1 signatures retain their original canonical bytes. Payload identity hashes
still cover the complete canonical JSON. Signed result reports remain v1;
receiving a result does not confer v2 Write permission.

## Bounded compensation

The reviewed Plan includes Contract `rollback.deadline` and environment
`rollbackTargetHash`. The latter commits to the Runner-local ECS region,
cluster/service, predecessor task definition and container digests. The customer
computes it with `cmd/rollbacktarget`; no caller-supplied provider target enters
the Grant. Other adapters without this target attestation cannot use this path.

After source success is durable, a rollback Grant and independently provisioned
manifest bind `source_grant_id`, `source_nonce`, `source_request_hash`,
`target_hash`, `source_issued_at`, `valid_until`. Source issue time starts the
Contract window conservatively, before dispatch; failure detection, response
reception and restart cannot reset it. Grant expiry is capped by this deadline
and all original current authority. The manifest CLI requires `-source-grant`.

Both issuer and Runner require an exact known succeeded deploy, matching Run,
step, Plan, artifact, target and external execution. Runner checks the approved
local predecessor hash before and after credential acquisition. Existing current
identity/delegation, Context, Policy and approval checks still apply. Unknown
source, unknown target, revocation or deadline expiry produces zero new Write.
Workflow exposes `COMPENSATION_UNAVAILABLE` or `COMPENSATION_EXPIRED` where
applicable; expired forward authority instead reports reauthorization required.

Runner reservation atomically consumes `(tenant, runner group, source nonce)`.
Identical operation replay restores existing truth; another key/Grant for that
source cannot obtain a second compensation Write. Consumption is conservative:
a failure after reservation does not release permission, even if credentials or
Write later fail. Reconcile and escalate; never clear the fence to retry.

## Persistence, compatibility and recovery

Control Plane migration 14 and Runner migration 008 add a source nonce column
and partial unique index. The Control Plane down migration refuses to discard
populated consumption state. Run JSON persists `authorization_stop`.

Temporal's `explicit-authorization-expiry-stop` version marker preserves old
history commands. Known expiry before reservation stops explicitly; a previously
reserved or uncertain operation keeps read-only reconciliation semantics.
Existing v1 records gain no inferred epoch or compensation permission. Production
source-enabled Runner refuses legacy Write Grants lacking an epoch. Drain old
issuers, inventory outstanding writes, apply migrations, deploy compatible
issuers/Runner/customer signers together, and provision fresh Plans/manifests.
Do not downgrade v2 to old Runners or edit old Plan times in place.

Authenticated signed results and source-bound observations of known operations
remain admissible after authority expiry. Recovery never resets nonce or creates
Write authority. After outcomes are known, an operator collects fresh Context,
plans only remaining work, obtains fresh approval/manifest and records the old/new
Run relationship. Automatic linked successors are not implemented.

## Evidence and alternatives

[Scenario evidence](../design/authorization-lifecycle-scenarios.md) separates
virtual time, actual PostgreSQL persistence and external validation. Real cloud
staging is unavailable. This does not establish production readiness.

Rejected: blanket TTL extension, silently re-signing later Plan times, arbitrary
rollback targets, delivery leases as authority, dropping late recovery evidence,
or retrying an UNKNOWN Write. Post-forward-expiry compensation would require a
separate fresh trusted Context and approval lifecycle, and is not enabled here.
