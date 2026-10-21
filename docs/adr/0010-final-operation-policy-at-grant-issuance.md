# ADR-0010: Evaluate and commit final operation policy at Grant issuance

- Status: Proposed (implementation submitted for review)
- Date: 2026-10-18
- Scope: Issue #48; extends ADR-0003/0008 without changing protocol v1alpha1

## Context

Workflow policy evaluation can precede approval. Its input hash consequently
does not include later approval proofs. Rollback is a separate capability from
deploy and needs its own decision. Copying the preliminary decision's hash into
a final Grant fails the Runner's correct canonical-input comparison.

Adding post-approval and rollback Activities would change Temporal commands and
require versioning and old-history tests. Updating the Run from inside another
Activity also changes its optimistic version without returning it to the
Workflow, risking a subsequent persistence conflict.

## Decision

Retain preliminary Workflow policy/approval handling. At the existing Grant
issuance boundary, load the durable Run and build `InputForOperation` including
the specific capability, pinned Context and final approval references. Use the
same configured policy engine/hash as planning and preliminary evaluation;
missing/changed policy or unready pinned facts refuse issuance.

Re-evaluate this final input. Only final ALLOW can issue a Grant; DENY and a new
REQUIRE_APPROVAL stop and require renewed operation authorization. Verify that
the returned input hash is the canonical hash. The Grant carries this final
hash, and its issue audit includes the complete final decision/hash and source
Run version. Grant, audit and dispatch outbox commit together before delivery.
The Run's preliminary Policy projection remains preliminary; final decisions
are independently retained in issue audit records.

Within the same store transaction/lock, check the authorizing Run version before
creating a dispatch. PostgreSQL holds a shared row lock through commit; Memory
checks under its mutex. A concurrent approval/Context change conflicts instead
of publishing the stale decision. The store record carries a transaction
precondition, not a new signed protocol field or database column. Zero remains
available only to existing low-level store/import paths; application issuance
requires a positive durable version.

Duplicate operation keys may return an existing Grant only when its final input,
action, target, Run and step match and it has not expired. A new approval cannot
silently reuse an old Grant. Existing independent Runner Context, approval,
signature, local policy, journal and hash checks all remain required.

## Alternatives

- Remove hash comparison: rejected; would discard the approval/action binding.
- Reuse deploy policy for rollback: rejected; operation requirements can differ.
- Recompute a hash without policy evaluation: rejected; integrity alone does not
  authorize the final input.
- Add Workflow Activities and mutate its Policy projection: unnecessary here;
  final issuance already has a durable audit/outbox transaction and is used by
  all execution entry points.

## Consequences and compatibility

No Workflow commands, Activity names/results or signed protocol schema change.
No Temporal version marker is needed. Existing completed histories replay
unchanged; they are not repaired. Old pre-issued Grants remain old objects and
are not re-signed or rewritten. Follow the migration runbook before unfreezing
affected groups. New final authorization does not revoke an already issued
Grant; existing manifest/revocation controls remain necessary.

Custom policy engines must expose their exact pinned PolicyHash. Local test
fixtures now pin the actual mandatory engine hash rather than an unrelated
placeholder. This decision does not authenticate synthetic identity or Context,
solve approval/compensation expiry (#51), or implement UNKNOWN convergence (#38).
Private Policy security remediation remains on its private reporting path.

## Verification

Connect actual Grant issuance to Runner final authorization with mandatory OPA:
pre-approval REQUIRE_APPROVAL, approved deploy, simulated verification failure,
separately evaluated rollback, and duplicate delivery without extra writes.
Re-signed mutations of approval, capability, artifact, Context and input hash
must obtain no credentials and perform no writes. Test stale approval, new
operation approval requirements, changed policy, duplicate keys and a Run
change during signing. Real PostgreSQL tests cover final decision persistence
and stale-version refusal. Provider operations are simulated; no live AWS claim.
