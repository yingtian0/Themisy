# ADR-0015: Process-local typed CI observation snapshots

- Status: Proposed
- Date: 2026-10-20
- Related: Issue #13, ADR-0003, ADR-0005, ADR-0006

## Context

An allowlisted CI read adapter returns four-valued source evidence, but no
collector currently retains that evidence with its tenant/logical target and
capture-time binding. A plain hash-addressed JSON object cannot prove source
authenticity: a requester can hash its own invented success record.

## Proposed decision

Add a customer-side CI-only collector that owns a copy of configured source
mapping, the actual read adapter and a verification-purpose credential broker.
It accepts no arbitrary provider selectors, URL or success/health flags. Its
private-field `CISnapshot` can only be minted by source collection in this
package. Exported records preserve source evidence and add typed target/time/
schema/hash binding. They cannot be imported back into a trusted snapshot.
The JSON method clears its receiver before refusing an imported record. Callers
must discard destinations on all decoding errors, including syntax failures
which the standard decoder can reject before invoking custom methods.

Fresh CI checks keep all four evidence statuses, require PASS with a strictly
unexpired source freshness window and check the snapshot hash. Exported
collections are copied so caller mutation cannot alter the retained observation.
Secrets and raw provider errors are absent from records. No authority is inferred
from an archive digest about a deployment image.

An optional audited collector commits the copied observation through the existing
context-aware audit sink before returning a snapshot. A locally generated event
correlation, the original four-valued status and complete inspectable record bind
the audit to the snapshot hash. Error, ambiguous acknowledgement, cancellation or
append deadline yields no returned snapshot, without retry or readback-to-allow.
Audit latency cannot extend the source freshness window. An audit record remains
unsigned inspection data and cannot be imported as a trusted snapshot. No new
schema, Plan/Grant correlation or audit retrieval surface is introduced.

## Alternatives

- Import records after hash verification alone: rejected because hashes do not
  authenticate their creator.
- Immediately assign a Context hash to one Policy constructor: rejected because
  Control Plane/Grant/Runner canonical inputs must change consistently, with
  persistence, invalidation and live-source verification.
- Reuse a previous PASS after collection failure: rejected; source failure remains
  INCONCLUSIVE or returns no snapshot, depending on whether evidence was produced.

## Consequences and limits

This is an opt-in process-local observation unit, disconnected from production
composition. It adds no durable authority or network protocol and does not replace
the PostgreSQL application state or customer Runner Journal. JSON records are
inspectable evidence data only. Cross-process trust, signed transport, retention,
Plan/Grant audit correlation and trusted restore require a subsequent reviewed
design. Observation-only audit storage is opt-in and does not supply that trust.

The Snapshot is CI-only. Runtime dependencies, Contract/Profile source metadata,
artifact provenance, identity, complete Context hash pinning, approval freshness
and Grant-time revalidation remain separate prerequisites. No existing Policy
hash/input comparison or Runner authorization is changed or skipped. This ADR
is proposed, not accepted; neither local fixtures nor a passing CI fact proves
the Issue #13 end-to-end production acceptance conditions.

## Validation

Tests compose the actual broker/CI adapter with synthetic HTTP/token fixtures,
verify no provider writes, preserve four-valued results, refuse stale/future/hash
mismatch and requester-like import, and check immutable config/record copies.
No live GitHub/runtime source or production Write validation is claimed.
Audited collection tests use synthetic append sinks, including lost response and
cancellation, preserve all four statuses and reproduce the record hash after
JSON serialization while refusing trusted import. An isolated PostgreSQL target
also validates all four states across store reopen, source/hash reconstruction
from JSONB and no returned snapshot on a server-refused INSERT. It requires a
dedicated fresh fixture, never a caller database. Lost acknowledgement and
deadline tests remain synthetic; a successful restore is not trusted import.
