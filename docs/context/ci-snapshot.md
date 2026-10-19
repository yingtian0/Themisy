# CI-only collected context snapshot

`contextsnapshot.NewCICollector` composes the existing GitHub read-only adapter
and verification-purpose credential broker in a customer-side collector. The
constructor copies the operator's target allowlist. `Collect` accepts a tenant,
logical service/environment and exact commit SHA, not a repository, workflow,
URL, success flag or credential. Invalid/unmapped requests fail before token
acquisition. A configured token source supplies read credentials only to the
provider boundary. No application/Runner deployment default instantiates this
collector yet.

The resulting `CISnapshot` holds a private immutable `CIRecord` with schema,
tenant, target, commit, capture time, the complete typed source evidence and a
hash binding all these fields. Source run/attempt, repository/workflow, artifact
archive IDs/digests, update/observation time, maximum age, reason, query hash and
original evidence hash remain intact. `Record()` exports an independent copy
for inspection. The hash is deterministic for the same exact record; a different
capture time or source observation produces a different snapshot. Hashing errors
return an error and no snapshot, never a hash of an empty serialization.

All four source statuses are retained. `RequireFreshCI(now)` succeeds only for
PASS within the original source-update-plus-maximum-age deadline, with valid
capture/observation ordering and an intact record hash. Equality at the expiry
boundary fails. The caller supplies a trusted current clock; a requester-selected
or historical observation time is not a way to renew evidence. FAIL, MISSING,
INCONCLUSIVE, stale/future/hash-mismatched and zero snapshots refuse the check.
Transport/rate-limit/partial-data observations stay INCONCLUSIVE. Credential or
invalid-request errors return no usable snapshot and expose no raw provider error.

## Process trust boundary

`CIRecord` is unsigned data, not a proof. Anyone can recompute an exported JSON
record's hash, so it must not be accepted from request bodies, files or an Agent
as trusted CI success. `CISnapshot` has no public value constructor or import
path. JSON import explicitly fails and clears any previous positive value.
Recollect through the configured source instead. The configured HTTP client and
token source are trusted components, not requester-selected plugins. Cross-process
transport/persistence needs its own reviewed authentication and freshness design.

The collector does not assert an artifact/image correspondence: GitHub archive
digests identify uploaded artifact archives, not ECS image digests or build
provenance. CI success also says nothing about dependency runtime health,
Contract/Profile provenance, identity/Delegation, Policy or Write permission.
`RequireFreshCI` is only a CI fact check, not authorization.

## Validation and remaining integration

Run `go test -race ./internal/contextsnapshot ./adapters/githubactions
./pkg/credentials`. Tests exercise the real adapter/broker with deterministic
HTTP/token fixtures, copied allowlists, exact target/commit/read-only requests,
credential isolation, four-state retention, expiry/future/hash failures,
failed JSON import clearing a positive value and deterministic context hashes.
They do not contact live GitHub or a runtime provider.

Planner/Policy/Grant/Runner constructors are unchanged. The legacy prototype CI
and dependency booleans remain unsuitable as production facts. Do not wire only
one side of the canonical Policy input or rewrite pinned hashes to make it pass.
Full Context Snapshot persistence, dependency observations, metadata provenance,
artifact provenance, Plan/Grant hash binding, approval invalidation and live
staging scenarios remain open under Issue #13. See proposed
[ADR-0015](../adr/0015-ci-only-context-snapshots.md).
