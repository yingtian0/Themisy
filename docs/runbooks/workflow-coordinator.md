# Control Plane workflow coordinator operations

The workflow coordinator is part of the Control Plane PostgreSQL database. It
does not share tables or credentials with a customer's Runner journal and does
not replace Runner-side authorization.

## Before starting workers

1. Back up the Control Plane database and apply migrations with the migration
   identity while following the normal deployment procedure.
2. Configure all Temporal workers to use the same Control Plane database and
   identical scheduler limits and production breaker policy.
3. Keep one worker until the dedicated PostgreSQL integration suite has passed
   against a disposable database. That suite must use independent Store pools
   to prove contention, lease expiry/fencing, UNKNOWN capacity retention,
   idempotent completion, and breaker persistence across store restart.
4. Confirm each worker reports coordinator errors as blocked/fail-closed. Never
   switch a production worker to an in-memory budget after a database error.

## Restart and unresolved operations

Before restarting workers, record open circuit state and the age/count of
`RESERVED`, `ADMITTED`, and `UNKNOWN` reservations. A reservation that has not
been admitted can be reclaimed after its lease expires; an admitted or unknown
reservation cannot. Do not delete reservation, scope, admission, outcome, or
counter rows to clear a block. Reconcile the external provider state through
the approved recovery path, then record one terminal result. If state remains
ambiguous, preserve `UNKNOWN` and keep capacity reserved.

If shared PostgreSQL is unavailable, dispatch must stop. Restore connectivity
and verify the stored counters and circuit state before resuming. If a restore
or migration would remove unresolved reservations, stop and reconcile them
before proceeding. The down migration deliberately refuses to drop unresolved
state.

## Audit and verification

Each reservation, write admission, block, quarantine, completion, and unknown
transition writes to the append-only audit stream in the same transaction as
coordinator state where the transaction owns that state. Preserve those events
with the database backup. Inspect `workflow_schedule_reservations` and linked
scope/admission/outcome rows read-only; do not repair counters with ad hoc SQL.

Use a dedicated disposable PostgreSQL database for the multi-store acceptance
test. The repository's general integration migration test applies down
migrations and must never be pointed at a shared or valuable database.
