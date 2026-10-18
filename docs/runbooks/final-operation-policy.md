# Final operation policy migration

Issue #48 now evaluates the final operation at Grant issuance. Its input includes
the durable approval references and deploy/rollback capability. The issue audit,
Grant and outbox atomically retain the final decision/hash, separate from the
Workflow's preliminary step Policy projection. See [ADR-0010](../adr/0010-final-operation-policy-at-grant-issuance.md).

Before upgrading, freeze affected Runner groups and inventory outstanding,
leased, in-flight and UNKNOWN Grants. Drain old Workers and Control Planes that
can still issue Grants using preliminary hashes. Already signed Grants are not
rewritten: fence or expire outstanding old authorizations using the supported
[manifest/recovery process](runner-recovery-and-manifests.md), and reconcile
existing writes read-only. Do not remove hash verification or clear reservations.

Configure the same policy engine/hash for planning, preliminary evaluation and
issuance. The standard composition root does this. A missing engine, different
pinned policy, invalid/expired/revoked approval or unavailable pinned execution
facts prevents a new Grant. A final rollback decision of REQUIRE_APPROVAL is
not satisfied by blindly reusing deploy authorization; issuance stops. Obtain
the required fresh operation authorization through the supported approval flow.
If that flow is not available, keep the operation stopped for operator handling.

Each issue transaction checks the durable Run version. Concurrent changes cause
a conflict; re-read state and re-evaluate rather than reuse the previous signed
payload. Same-input duplicates retain the same Grant. A changed input/approval
on the same operation key or an expired original Grant requires reconciliation
or renewed authorization; changing the key is not permission to repeat a Write.

This does not revoke previously issued Grants, create trusted Context/identity,
renew an expired Plan or extend rollback authority. Existing manifest fencing,
Runner approval checks, read-only reconciliation and audit procedures remain
required. Plan/approval/Grant/compensation lifecycle is tracked in #51.

No Workflow command graph or Activity result type changes. Historical Activity
results and completed Runs are not repaired. The generic two-stage authorization
test uses real Grant/Runner code and OPA, with simulated identity, independent
Context/approval fixtures, credentials, adapter and failed observation. It is not
a live ECS/health/identity deployment. Real PostgreSQL verifies issue/audit commit
and version conflict separately; CI runs that test in the disposable database.
