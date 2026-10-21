# Memory-worker restart and unresolved operations

This procedure covers the memory-store development mode, whose scheduler
reservations, budgets and breaker windows are process-local. PostgreSQL-backed
Workers use the durable coordinator described in [capacity](capacity.md) and
[circuit breaker](circuit-breaker.md).
A single Worker is a development restriction, not a restart-safe or production
guarantee. One task queue, one deployment replica setting, or durable Temporal
history does not persist these maps. Do not claim multiple-Worker failover or
global service exclusion until #50's real shared-PostgreSQL/process tests pass.

## Before stopping a development Worker

1. Stop admitting new release requests through the deployment's existing
   controls and freeze affected Runner groups with authenticated operator
   controls. Pause affected Runs where supported. Pausing/Worker shutdown does
   not revoke an outstanding Grant or interrupt a provider Write.
2. Record affected tenant/environment/service/failure-domain scopes, active Run
   and Execution IDs, signed Grant/delivery status, external IDs, UNKNOWNs and
   incident/breaker audit evidence. Keep sensitive records private; do not store
   credentials or customer logs in public Issues.
3. Ensure old Workers are drained/stopped before starting a replacement. Do not
   use a rolling overlap or scale out as a way to recover process-local locks.
4. Inventory/fence outstanding authorizations using the
   [Runner manifest/recovery runbook](../runbooks/runner-recovery-and-manifests.md).
   A frozen group and an expired delivery lease are not proof of no external Write.

## After replacement

Treat the coordinator as empty and its previous safety state as unknown. Keep
affected groups frozen while comparing Control Plane records, customer Runner
Journal and read-only provider observations. Retain unresolved reservations
conceptually as blocking; do not dispatch a different Run to the same target or
shared failure domain merely because the new Worker has capacity.

Authenticated signed recovery reporting may repair the existing Grant truth
after expiry. The versioned recovery Workflow can converge the existing Run after a verified
terminal result, while legacy/unsupported records remain blocked. Do not manually set a Run to
success or reset nonce/idempotency reservations. Do not re-Write to discover the
outcome. If prior breaker samples/open state cannot be established, an empty new
process window does not authorize closing the incident or unfreezing production.
Current public interfaces intentionally have no Agent-facing breaker reset.

Only an authorized operator can decide when the recorded scopes are reconciled
and the development exercise can resume. Document that decision and its evidence.
If a supported recovery operation cannot express the necessary decision, stay
stopped; do not edit database rows or temporarily remove checks. A single Worker
still does not convert this setup into a production-supported deployment.

## Durable-mode acceptance boundary

Before enabling multi-Worker operation, persist Control Plane reservations and
budget/breaker decisions in one transactional boundary, with owner tokens/CAS,
fencing of old owners and idempotent outcome consumption. Lease expiry must not
free unresolved writes. Tests must use two independent processes and real
PostgreSQL, restart/redistribution, duplicate/lost completion, open breaker
retention, UNKNOWN/expired lease/old-owner return, and store-outage refusal/audit.
This is distinct from customer Journal ownership of a single provider operation;
do not repurpose or reimplement #8 as the global coordinator.

The memory-mode procedure does not provide durable state or automatic reset.
Use the integrated PostgreSQL coordinator and its isolated tests for durable-mode
validation; neither mode is a production-readiness claim.
