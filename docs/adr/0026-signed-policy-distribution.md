# ADR 0026: Scoped signed Policy rollout and fail-closed enforcement

- Status: Proposed
- Date: 2026-10-28
- Scope: #10; no Policy catalog object is authorization by itself

## Problem

Signed bundles and offline candidate/shadow tools exist, but there is no active
per-tenant/Runner-group assignment, acknowledgement or rollback lifecycle.
Runner startup compilation alone also cannot enforce subsequent expiry or key
revocation. A lower layer must never cancel an upper-layer denial or approval.

## Decision

Keep the immutable, hash-addressed candidate catalog separate from active state.
Provide an authenticated policy-admin API backed by durable bundle and scoped
assignment records. An assignment update requires its prior revision (CAS), is
audited atomically, and increments a monotonic revision. Canary rollout updates
one configured Runner group at a time. Pin both Bundle hash and the assignment
revision into Plan, canonical Policy Input, Grant and independent Manifest.
Returning to the same Bundle on rollback cannot revive an old approval. Rollback is an explicit update to the
previous still-valid signed bundle, never a lower revision or an unsigned alias.

Distribute through the existing authenticated outbound Runner connection. The
server derives tenant and Runner group from workload identity; it never accepts
caller-selected scope. The Runner independently verifies its customer-managed
Policy trust, signed bundle lifetime, local assignment revision fence and
mandatory baseline. Record delivery acknowledgements as acknowledgement only,
not proof that a cloud operation occurred.

Compile and evaluate the embedded mandatory baseline independently. Evaluate
each populated cumulative layer prefix and combine with deny precedence and
union of approval roles/reasons. A lower module can add constraints but cannot
remove an earlier prefix's result. Compile/evaluate with no network,
environment, nondeterministic or wall-clock Rego builtins.

Plans pin the active signed bundle hash. All selected groups of one Run must
resolve to that same hash; otherwise create separate Runs. Re-evaluate the
current assignment before Grant issuance and before Runner Write. Assignment
change, expiry, revoked trust or disagreement with the immutable Plan stops new
writes for replan/reapproval; no default/fallback bundle supplies authorization.
Read-only reconciliation and runtime observations remain available.

## Consequences and validation

New bundles use a domain-separated SHA-256 signing message
(`themisy.policy-signature/v2`) so reviewed Rego source does not exceed KMS raw
message limits. The full canonical Bundle hash is still verified. Legacy signed
bundles remain verifiable, with the embedded baseline enforced independently.
Older strict decoders cannot read v2; quiesce and upgrade the fleet before
assignment. Legacy unscoped prototype bundle rows are not imported as authority.

Policy authoring remains GitOps: review signed source in a PR, run Rego/fixture,
Reason Code and shadow comparisons, then register and explicitly assign. This
service does not automatically merge policy source or invent rollout success.
The deployment needs a customer-controlled Policy issuer and fresh trust bundle.

Use clocks, scoped canaries/rollback, conflicting CAS, restart/replay fences,
upper-deny/lower-allow and expiry/tamper cases to validate. Provider calls must
remain zero when policy verification fails. No live cloud rollout evidence is
claimed by fixture scenarios. Existing production writes must be quiesced while
migrating; old plans do not silently adopt a new bundle or assignment.
