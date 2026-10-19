# ADR 0012: Durable Control Plane workflow coordinator

- Status: Proposed
- Date: 2026-10-19

## Context

The in-process scheduler and circuit breaker coordinate workflows handled by
one process only. Multiple Temporal workers can therefore independently spend
the same tenant, region, service, or failure-domain capacity and can disagree
about production circuit state. A Control Plane reservation is not the
customer Runner journal and must not grant provider credentials or replace the
Runner's final policy decision.

## Decision

Store Control Plane schedule reservations, capacity counters, owner tokens,
fencing values, write admissions, outcome history, and production circuit
samples in the shared PostgreSQL database. Acquire all capacity dimensions in a
deterministic order in one transaction. Persist write admission before the
existing durable execution reservation and provider call. A write may proceed
only with the current owner token and fencing value. Expiry can reclaim an
unadmitted reservation; it cannot authorize another write after admission.

An ambiguous result is `UNKNOWN`. It remains unresolved, keeps all capacity
scopes active, and blocks another reservation. A later authorized
reconciliation can record one terminal outcome and release capacity. Duplicate
completion does not add another breaker sample. Database loss or incompatible
configuration fails closed. Existing Temporal histories use a version marker
to preserve their prior command payloads; legacy executions without a durable
lease are quarantined before any new provider call.

## Alternatives considered

- Keep worker-local scheduler state: rejected because independent workers can
  oversubscribe shared limits and lose circuit history on restart.
- Serialize one worker on a task queue: rejected as a deployment-only
  convention that does not fence stale owners or make counters durable.
- Use a separate coordination service: deferred because PostgreSQL is already
  the durable Control Plane authority and supports transactional reservations.

## Consequences and safety

The Control Plane now requires the shared coordinator migration before workers
run this workflow version. Scheduler policy must be consistent across workers.
Operational recovery must preserve unresolved rows and audit history. This
mechanism controls Control Plane admission only; customer Runner journal,
credentials, and local policy enforcement remain separate and authoritative
for provider writes. The acceptance suite must exercise independent Store
instances against a dedicated PostgreSQL database, including contention,
expiry fencing, UNKNOWN retention, duplicate completion, and circuit restart.
Until those real-database tests pass, this ADR remains Proposed and multi-worker
enablement is not accepted.
