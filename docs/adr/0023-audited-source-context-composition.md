# ADR-0023: Audited source observation composition

- Status: Proposed
- Date: 2026-10-24
- Related: Issue #13, ADR-0015, ADR-0017, ADR-0018

Individual opaque CI, dependency and metadata snapshots cannot be treated as one
context until their tenant/service/environment and application repository agree.
Add an opt-in assembler accepting only collector-minted snapshots. Bind the CI
application commit, metadata source selectors and the complete service dependency
set derived from the trusted Contract. Metadata repository revision is distinct
from the application's CI commit. No caller-provided list or health boolean can
replace required observations. A dependency-free Contract may omit a dependency
snapshot; that exception comes from trusted metadata, not a caller flag.

The assembled record includes all covered component records and canonical hashes,
with expiry capped by the earliest source expiry. Audit the independent record
before return, then recheck freshness/cancellation. Imported JSON cannot restore
trust. Failed/ambiguous append or expiry returns no usable context, even when an
inspection event was already committed. No new database owner or migration is
introduced. Records describe observations, not approval or deployment success.

This is SOURCE_OBSERVATIONS_ONLY: it does not attest artifact/image provenance,
resource dependency health, requester/Delegation or complete Policy context.
Existing Planner options can represent the hash, but production defaults,
Approval invalidation and signed Grant/Runner binding are not changed here.
Those remaining Issue #13 gates and live sources still need implementation and
acceptance. The ADR remains Proposed pending independent maintainer review.

Tests collect all components through real typed adapters with synthetic HTTP and
credentials; cover mixed scope/repository/commit, complete dependency sets,
non-PASS/stale sources, leaf Contracts, independent copies, deterministic hashes,
canonical Planner hash binding, failed/cancelled/expired audit and JSON rejection.
The dedicated PostgreSQL audit target verifies full record/hash retention after
reopen and a server-refused INSERT without returning a context.
