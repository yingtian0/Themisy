# ADR-0022: Separate read-only Runtime observations

- Status: Proposed
- Date: 2026-10-24
- Related: Issue #9, ADR-0005, ADR-0021

The current ECS gate checks real rollout/task health but discards the observation
summary. Add a read-only observation method that reuses that exact gate and
registered definition preflight. Export confirmed desired/running/pending/task
counts and rollout state, plus logical target/operation, source and selector/
definition/container-set hashes. Unconfirmed provider state is INCONCLUSIVE,
never an invented successful runtime. Omit raw provider errors and resource ARNs.

Provide an opt-in tenant-bound collector with trusted ECS adapter, read-only
credential broker, maximum age and durable audit sink. Validate the logical
selector before acquiring credentials. Commit a distinct runtime_snapshot audit
record before returning an opaque observation snapshot. Fail closed on failed
or ambiguous append, cancellation, clock reversal and expiry. Stored JSON is
inspectable data and cannot restore trusted snapshots. Caller Run/Grant labels
are correlation, not an authenticated Grant or execution authorization.

The existing audit store remains the owner; no new database schema or public
command is introduced. The collector requires a separately configured verify-
purpose read role. It is not connected to Runner execution, signed Result/Evidence
or Workflow defaults by this unit. Separate metric Evidence storage remains
unchanged. Signed runtime attestation, workflow composition and live AWS gates
remain #9/#11 work. This ADR is Proposed, not maintainer acceptance.

Synthetic SDK tests prove no UpdateService for deploy/rollback observations,
confirmed versus unconfirmed semantics, hashes and clock rejection. Collector
and dedicated PostgreSQL tests cover credential/audit/cancellation/expiry failure,
JSON import rejection, source redaction and persistence/reopen/refused INSERT.
