# Shared Runner journal and independent plan manifests

## Trust and storage boundaries

All replicas in a tenant/Runner group use the same customer-managed PostgreSQL
database. Runner connections explicitly select only the `themisy_runner` schema;
they never fall back to `public.runner_journal`. Control Plane migrations and
Runner migrations are separate. Memory is development-only.

Run `go run ./cmd/runner -config <runner.yaml> -migrate-journal` with a migration
identity before starting runtime replicas. Apply `deploy/runner-journal-role.sql`
as schema owner and grant that NOLOGIN role to the workload's login. Runtime
must not own tables or inherit the migration role. Use TLS with certificate and
hostname verification (`sslmode=verify-full`) outside isolated local tests.

Existing installations using `public.runner_journal` must stop every Runner and
copy ALL journal rows and append-only audit events into the new schema before
starting any replica. Preserve nonce, idempotency key, state_version, owner and
terminal results. Never start with an empty journal for a previously active
Runner group. Take and verify a backup first; migration is deliberately not an
automatic destructive move of Control Plane data.

## Recovery semantics

1. Commit nonce/idempotency reservation and its audit before obtaining credentials.
2. Commit DISPATCHED intent using state_version before invoking the provider.
3. Never transfer write authority because a delivery/recovery lease expired.
4. Claim read-only recovery using row locks, SKIP LOCKED and a version increment.
   A claim fences an original RESERVED writer before dispatch. Claims do not
   authorize writes. A slow expired claimant cannot commit over a newer claim.
5. Reconcile the external runtime. Absent, ambiguous or failed observations stay
   UNKNOWN with an audit event; absence never authorizes another UpdateService.
6. Report saved results through the authenticated `/v1/runner/grants/{id}/reconcile`
   route. It compares the canonical request hash and run/step against the Control
   Plane dispatch. A replacement peer in the same tenant/group can report after
   the original Grant expired, without persisting delivery tokens in the journal.
7. Record successful reporting durably. Lost ACKs are retried idempotently. A
   conflicting terminal Control Plane result stays unchanged and is audited.

The external provider is not transactionally coupled to PostgreSQL: the guarantee
is at-most-once dispatch, not exactly-once successful execution. A crash before
the provider receives the request can leave an action unresolved indefinitely.
Operators must not delete reservations to retry; investigate and explicitly
authorize a new action. Runtime health and signed Result/Evidence remain #9 scope.

## Independently approved manifests

Configure `manifests.file`, `issuer`, `trust_key_id` and `trust_key_file` on the
Runner. Use an approval-authority key that the Grant dispatcher cannot use.
Create a signed manifest from durable approved run/step state with:

```sh
go run ./cmd/planmanifest -run RUN_ID -step SERVICE -revision 1 \
  -issuer https://approvals.example -kms-key-id APPROVAL_KMS_KEY
```

The command reads `THEMISY_DATABASE_URL` and emits a JSON array to stdout. Deliver
that file through the customer-controlled configuration/approval deployment
path, using atomic file replacement. A protected local development key is an
explicit alternative, not the production default. Do not obtain the manifest
from an Action Grant, and do not give the Grant dispatcher the provisioning key.

The Runner verifies version, canonical signature, issuer, tenant/group and time
window, then pins the manifest in PostgreSQL. Every execution checks its Plan,
Contract, Profile, Policy, Evidence, target, action and approval references.
The file is refreshed before accepting each delivery; file errors fail closed.
Shared stored payloads are signature- and expiry-checked on read, not blindly
trusted because they came from a database.

Increment the revision for a changed plan. Provision all steps/operations at the
new revision before issuing new Grants. Activation of any higher revision makes
all old-revision manifests inaccessible for new execution; old manifests cannot
be reinstalled. Same-revision changes are rejected. Keep only the latest revision
for each run in the provisioned file. Rollback is a separately approved typed
operation, including its original external execution ID.

This establishes independent pinning, not independent proof that CI or human
identity claims are true. Authoritative fact collection and IdP roles remain
#13/#12. The existing approved-state source must be protected accordingly.

## Operations and disaster recovery

- Monitor UNKNOWN age, recovery claims, reporting backlog, DB availability,
  conflicts, rejected manifest revisions and manifest expiry.
- Rotate DB credentials via workload secret delivery and restart pools/replicas;
  a failed reconnect must pause writes, not select memory storage.
- Back up the entire Runner schema (journal, revisions, manifests and audit)
  with pg_dump and retain WAL/PITR according to the customer's recovery policy.
- Restore into an isolated database and verify uniqueness and revision fences
  before reconnecting any Runner. A stale backup cannot prove that writes after
  its snapshot did not happen. Freeze all replicas, recover WAL to the agreed
  point and reconcile provider/Control Plane history; do not automatically resume.
- Apply migrations once with the migration role while replicas are drained;
  migrations use a transaction and advisory lock. Do not drop journal columns or
  reset uniqueness to work around a failed migration.
- Roll out the same trust configuration to all replicas before activating new
  manifests. Expired/missing approval material means zero new writes.

## Verification

`make test-integration` runs real PostgreSQL/Temporal tests plus separate-process
Runner contention/crash tests. The provider side of these fault tests is a
durable counting test adapter, not live AWS. Dedicated AWS staging/IAM evidence
is still tracked by #11; do not describe these tests as a live ECS deployment.
