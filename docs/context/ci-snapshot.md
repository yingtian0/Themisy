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
as trusted CI success. `CISnapshot` has no public constructor for a verified value
or trusted import path. Its JSON method rejects imported records and clears the
receiver when invoked. Always discard a decoding destination after any error:
`encoding/json` can reject malformed syntax before invoking custom methods.
Recollect through the configured source instead. The configured HTTP client and
token source are trusted components, not requester-selected plugins. Cross-process
transport/persistence needs its own reviewed authentication and freshness design.

The collector does not assert an artifact/image correspondence: GitHub archive
digests identify uploaded artifact archives, not ECS image digests or build
provenance. CI success also says nothing about dependency runtime health,
Contract/Profile provenance, identity/Delegation, Policy or Write permission.
`RequireFreshCI` is only a CI fact check, not authorization.

## Validation and remaining integration

### Audited observation collection

`NewAuditedCICollector(collector, sink)` adds an opt-in audit commit gate around
the same collector. The sink must implement `AppendAuditContext` with a durable
commit before returning nil, respecting the supplied context. The context-aware
PostgreSQL audit store introduced by #12 provides this interface; an in-memory
or synthetic sink is test evidence only. No new storage schema is required.

Each collected observation appends `context.ci.observe`, with its original
four-valued status, snapshot hash, capture timestamp and complete copied
`CIRecord`. The record retains source selectors/revision, external run/artifact
identities, source/observation times, age limit, reason and content hashes.
The correlation ID is generated locally and cannot be chosen by the request.
Neither a credential nor a raw provider error is added. Invalid requests or
credential failures return no collected observation and do not append this event.

An append error, ambiguous acknowledgement, cancellation or elapsed append
deadline returns no snapshot and a stable error. There is no retry or readback
to infer success; an ambiguous append can therefore leave an observation in the
audit even though collection returned no data. Appends have a five-second upper
bound and honor a shorter parent deadline. Audit latency does not renew source
freshness: callers must still check `RequireFreshCI` with a current trusted clock.

Audit JSON can reproduce the recorded content hash for inspection. It cannot
recreate a trusted `CISnapshot`, including after a successful database restore.
This unit does not link an observation to a Plan, Approval or Grant, and does
not create a new audit retrieval endpoint or change its access controls. It
does not establish full Context persistence or production source authenticity.
Tests use a synthetic sink for lost acknowledgement, cancellation and append
deadline. `make test-ci-observation-audit` creates a dedicated temporary
PostgreSQL database, migrates only a fresh schema, persists all four states,
reopens the real store and reproduces the snapshot hash from JSONB. A separate
read-only client session refuses INSERT and returns no snapshot. The script
ignores caller database settings, uses a dynamic loopback port and deletes only
its own container and volumes. It does not contact a live CI provider.

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
