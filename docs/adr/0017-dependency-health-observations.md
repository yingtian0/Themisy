# ADR-0017: Typed read-only dependency health observations

- Status: Proposed
- Date: 2026-10-22
- Related: Issue #13, ADR-0004, ADR-0015

## Context

The pre-dispatch Context needs dependency observations obtained from a configured
observability provider. A request's `dependencies_healthy` flag or an invented
hash cannot establish runtime health. Deployment verification and dependency
observation also have different inputs: a dependency read must not require a
caller-supplied deployment execution ID.

## Proposed decision

Add an opt-in Datadog `HealthReader` separate from the existing deployment
verifier. An immutable operator catalog binds tenant/service/environment to a
supported site, expression, comparator/threshold, aggregation, minimum points,
window and maximum age. The caller selects only a logical target. Read credentials
are supplied by trusted customer-side composition, not an API request. No
production composition or credential broker is added by this unit.

Issue one bounded GET to the configured metrics v1 endpoint. Validate the echoed
query and millisecond bounds against the requested second-resolution window.
Keep PASS, FAIL, INCONCLUSIVE and MISSING distinct. Each series independently
needs enough unique, finite, in-window samples and a strictly fresh latest sample.
A partial, null, duplicated, out-of-window or stale series cannot be hidden by
another series. Classify provider/auth/rate-limit/response failures without raw
errors. Cancelled reads and credentials expired during collection return no
observation. Recheck conclusive-result freshness at capture time; latency cannot extend it.

Return a schema-versioned DTO with target/source binding, query-configuration
hash, window, capture time, the oldest per-series latest timestamp, maximum age,
canonical timestamp/value sample hash and full typed-record hash. Sort samples
and per-series hashes for order independence. Raw responses, query expressions
and keys are not exported. These hashes detect content changes, not authenticity.
The returned DTO is freely constructible and must never be restored as trusted
Context or used alone to issue a Grant.

## Alternatives

- Accept caller booleans: no authenticated source binding.
- Reuse a deployment execution identity: conflates pre-dispatch dependencies
  with a completed deployment and does not supply a complete Context.
- Aggregate all series into one healthy result: loses per-series completeness
  and freshness; each returned series must pass its configured threshold.
- Wire this read directly into Policy/Grant issuance: deferred until collection,
  immutable snapshots, audit/persistence, approval invalidation and Runner hash
  revalidation are designed and reviewed together.

## Consequences and limits

No existing verifier, request schema, default API, Policy input, Grant, Runner
protocol or production authorization changes. This is one source adapter, not a
complete #13 implementation. It does not prove deployed revision, image digest,
CI, service dependency membership, identities, or Contract/Profile provenance.
A future collector must resolve the expected dependency set from trusted source
configuration, bind the broker and snapshot, and retain/audit observations before
Plan/Grant integration. A zero-dependency or partial set is not implicitly healthy.

This ADR remains Proposed. Local synthetic fixtures are not live Datadog staging
acceptance, and merging the opt-in implementation does not accept this design
or establish a production trust chain.

## Validation

Fixture tests exercise the real reader with fixed time and synthetic read
credentials/HTTP responses, four-valued results, exact query/window/tenant
selection, per-series freshness/completeness, cancellation, credential expiry,
provider failures and deterministic content hashes. No provider writes or live
credential reads are performed. Production staging remains outstanding.
