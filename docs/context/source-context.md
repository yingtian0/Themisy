# Audited source observation context

Create a `SourceContextAssembler` with a context-aware durable audit sink, then
call `Assemble` with the opaque CI, dependency and metadata snapshots returned
by their trusted collectors. No JSON record, success flag, caller-selected
source or dependency list can mint one of these snapshots.

The assembler requires fresh PASS CI and fresh Contract/Profile metadata for
the same tenant/service/environment. The Contract's application repository must
match the CI repository, expressed as `owner/repo`, `github.com/owner/repo` or
`https://github.com/owner/repo` exactly. Other URL/ref forms are refused. The
metadata configuration repository/commit identifies the source of the documents;
it may differ from the application repository/CI commit. These revisions are
recorded separately and never silently substituted for one another.

Every distinct service dependency in the trusted Contract requires a fresh PASS
health observation, in the same tenant/environment and requesting-service scope.
Missing, extra, duplicate or different targets fail. Multiple Contract edge types
for one dependency share that service's health observation. Resource dependency
health is outside this coverage. When the Contract has no service dependencies,
a zero dependency snapshot is accepted; an extra one is refused. No caller flag
can assert that dependencies are unnecessary.

The record contains the full covered observations, source selectors/hashes,
sorted service dependency set, capture time and earliest source expiry. Identical
records/time produce the same hash. Changing a source selector or observation
changes it, even if the Contract's semantic content stays equal. `Record()` and
the audit sink receive independent copies, including nested artifacts/evidence.
Imported or stored JSON cannot recreate a trusted context. Use
`RequireFreshSources(now)` at each point of use; expiry equality, zero, stale or
non-PASS sources fail. An audit result of OBSERVED is not an authorization.

One `context.sources.observe` / `source_context_snapshot` audit is committed
before return. Failure/response loss, cancellation or expiry during append
returns no context and never retries. A committed inspection event may remain
after an ambiguous response or later expiry. Source credentials, raw query,
raw documents and provider errors remain absent from the component records.

This mode is SOURCE_OBSERVATIONS_ONLY. It does not complete artifact/image
provenance, identity/Delegation, resource health, final Policy input or execution
permission. The canonical Planner already supports a ContextSnapshotRef and tests
demonstrate its hash binding; this unit does not wire production planning,
Approval invalidation, signed Grant fields or Runner validation. Existing CLI,
server and enforcement defaults remain unchanged. Those are separate #13 gates.

Run `go test -race ./internal/contextsnapshot` and
`make test-ci-observation-audit`. Synthetic provider fixtures exercise actual
typed collectors; the latter uses a fresh dedicated PostgreSQL fixture for
record/reopen/hash and refused INSERT. No live provider acceptance is implied.
See [proposed ADR-0023](../adr/0023-audited-source-context-composition.md).
