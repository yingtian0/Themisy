# ADR-0009: Share a customer-environment PostgreSQL journal across Runner replicas

- Status: Accepted
- Date: 2026-09-08

## Context

The Runner must reserve a nonce and idempotency key before an external write so
that redelivery, restart, and ambiguous provider responses do not cause a second
write. A journal stored only on one Runner's local filesystem cannot preserve
that property when several Runner replicas serve the same tenant and Runner
group. A replacement replica would not see the first replica's reservation.

The Control Plane PostgreSQL projection is not an acceptable substitute. It is
outside the customer Runner trust boundary, may be unavailable during a
Control Plane incident, and is rebuildable query state rather than the Runner's
write authority.

## Decision

Production Runner replicas in the same customer trust boundary use a shared,
customer-managed PostgreSQL database as the authoritative Runner journal. The
journal is logically separate from Control Plane application state and
projections. It uses a dedicated database or schema, migration set, and
least-privileged database role even when an enterprise self-hosted deployment
places both workloads on the same physical PostgreSQL cluster.

The journal enforces uniqueness for `(tenant_id, runner_group, nonce)` and
`(tenant_id, runner_group, idempotency_key)` in PostgreSQL. Reservation and its
audit record commit in one transaction. All state transitions use an expected
`state_version` and fail on conflict. Database unavailability prevents new
writes.

Only the replica that wins the initial reservation may begin the provider
write. A different replica must not take over or repeat that write after a
lease timeout because the first replica may have reached the provider. Other
replicas may perform read-only reconciliation of `RESERVED` or `UNKNOWN`
records and record the observed result using compare-and-set state transitions.

Runner identity is recorded on reservations and reconciliation attempts for
audit and diagnosis, but correctness depends on database constraints rather
than process-local locks. Credentials and delivery tokens are never stored in
the journal.

## Alternatives considered

### One SQLite or local file journal per Runner

Rejected for production Runner groups with more than one replica. It survives
one process restart but cannot prevent two replicas from accepting the same
logical action after redelivery or failover.

### Use the managed Control Plane database as the Runner journal

Rejected as the default because it collapses the customer execution and SaaS
trust boundaries. A self-hosted installation may share physical PostgreSQL
infrastructure only with separate logical storage, roles, and authorization
semantics.

### Allow a new replica to repeat a write when reconciliation finds nothing

Rejected. Provider observation may be stale and the original write may still
complete. An unresolved reservation remains fail-closed until an operator
explicitly authorizes a new action.

## Consequences

- A production Runner deployment now depends on highly available PostgreSQL
  inside the customer environment.
- Multiple replicas can share nonce, idempotency, and reconciliation state and
  can scale without partitioning actions by local disk.
- PostgreSQL outage pauses new Production Writes but does not grant permission
  to bypass the journal.
- Runner-only migrations and a least-privileged database role must be packaged
  separately from Control Plane migrations.
- Reconciliation work requires safe row claiming so several replicas can scan
  pending records without duplicate provider reads or conflicting updates.
- Backup, restore, retention, TLS, credential rotation, monitoring, and schema
  upgrade procedures become part of the Runner operational runbook.

## Safety implications

PostgreSQL provides durable mutual exclusion for Runner replicas, not exactly
once delivery by the external provider. The system guarantees at-most-once
write dispatch for an authorized journal record and resolves ambiguity through
read-only reconciliation. Projection state, a transport lease, or the absence
of an immediately visible ECS change never authorizes another provider write.
