# Trusted dependency observation collection

`contextsnapshot.NewDependencyCollector` composes the read-only Datadog health
reader with the existing Datadog credential broker and a context-aware audit
sink. Configure dependency sets by tenant, requesting service and environment.
Each set is nonempty, bounded to 128 unique dependencies and must reference
operator-configured health targets in that same tenant/environment. Missing or
empty sets are errors, never implicit health. Configuration is copied at construction.

`Collect` accepts only that logical key. It sorts and collects the complete set;
callers cannot supply a dependency list, query, provider site, threshold,
credential or health assertion. A credential acquisition failure discards the
partial collection. Provider FAIL, MISSING and INCONCLUSIVE observations remain
in the per-dependency evidence, including partial provider availability.

The returned snapshot owns a private record. `Record()` and audit records contain
independent copies. Records contain source/query/sample hashes, observation
windows, freshness, reason codes and individual statuses, without raw queries,
credentials or provider errors. The sorted evidence and capture time are bound
by the record hash. Identical records hash identically; a later observation has a
new capture time and may have a different hash.

Every complete collection appends one `context.dependencies.observe` audit event
before returning. The sink must commit before returning nil; use the PostgreSQL
store in durable compositions. Audit failure, response loss or cancellation
returns no snapshot and is not retried. The aggregate audit result is PASS only
when every dependency is fresh PASS at capture; otherwise it is INCONCLUSIVE.
The individual four-valued results remain authoritative for inspection.

`RequireFreshDependencies(now)` must succeed at the point of use. It rejects zero
snapshots, non-PASS evidence and expiry (including equality at the expiry boundary).
A collection can expire while collecting or appending audit; collection success
alone is never a positive health check. Exported JSON and stored records cannot
be deserialized into trusted snapshots. Recollect from trusted sources.

This is an opt-in collection unit. Existing command defaults, Planner, Plan hash,
Workflow and Action Grant protocols are unchanged. A complete immutable Context,
Contract/Profile provenance, Plan/Grant pinning, approval invalidation and live
Datadog staging remain Issue #13 work. This API grants no execution authority.

Validation: `go test -race ./internal/contextsnapshot ./adapters/datadog
./pkg/credentials`. `bash scripts/test-ci-observation-audit.sh` also verifies
PostgreSQL persistence/reopen, reproducible hashes and rejected audit INSERTs for
dependency collections using its own fresh fixture. Local HTTP/token fixtures
are synthetic and do not establish live Datadog acceptance.
