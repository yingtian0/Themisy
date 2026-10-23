# ADR-0020: Opt-in durable Policy shadow observation audit

- Status: Proposed
- Date: 2026-10-23
- Related: Issue #10, ADR-0003, ADR-0005, ADR-0013

The existing read-only comparison can produce useful decision/reason/role
changes without a durable record. Add a separately composed comparator with a
host-owned clock, trusted issuer/key resolver and context-aware append sink.
Verify both Bundles, copy and hash the report, and commit an observation before
return. Recheck expiry and cancellation after append; do not retry ambiguous
writes or return unrecorded comparisons. Audit and caller own separate nested
values. No raw inputs or modules enter the report audit.

The existing PostgreSQL append-only audit store remains the durable owner; no
new tables or state machine are introduced. A local JSON export was rejected as
the durable acknowledgement mechanism. Automatic activation and report-based
approval are out of scope: zero differences means only an observation about the
provided cases. Stored reports are not reusable authorizations. Event labels
and case identifiers still require operator sanitization/access control.

This API is opt-in; existing offline comparison, command defaults and enforcement
paths are unchanged. It is not a signed policy distribution protocol or an
operational rollout proof. Independent review remains required; the ADR stays
Proposed. Tests cover real signed fixture evaluation, copy isolation, failed or
ambiguous append, cancellation/expiry/clock reversal and dedicated PostgreSQL
persistence/reopen plus refused INSERT.
