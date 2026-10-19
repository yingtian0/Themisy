# Durable Activity deployment results

Deploy and Rollback success payloads now contain a `deployment_result` JSON
string. Its envelope version is `1`, with the durable Execution ID and the full
typed `executor.Deployment`: external ID, start/finish time, Run ID, service,
environment, desired version and operation idempotency key. The rollback result
uses the deploy key plus `/rollback`. Existing JSONB storage needs no SQL schema
migration. The envelope is serialized explicitly so Memory and PostgreSQL
round-trips use the same representation.

On duplicate reservation, Activities validate execution status, ID, adapter,
Run/service/key, external ID, required timestamps and the durable Run's matching
service/version/environment. Rollback also reloads the original successful
deploy and checks that its supplied source is exactly the saved result. Retry
arguments never supply missing result fields. New executor implementations must
return the complete typed Deployment; incomplete results stop for reconciliation.

## Upgrade and old records

Drain old Workers before accepting new work with this representation. Inventory
active runs, pending Activities and in-flight/UNKNOWN operations, following the
[manifest and recovery runbook](runner-recovery-and-manifests.md). Do not interpret
Worker restart, delivery lease expiry or a persistence error as Write permission.

Old success payloads containing only `started_at`/`finished_at` are **not**
automatically migrated. A retry encountering one, an unknown result version,
missing fields or a binding mismatch returns non-retryable
`UNKNOWN_EXTERNAL_STATE` and performs no new executor Write. Keep the historical
record; do not fill it from Activity arguments, delete its reservation or edit
the database to force success. Reconcile against durable Grant/Runner journal
and read-only provider state under the existing operator process.

## UNKNOWN Grant recovery in the existing Run

When GrantExecutor times out, the Activity saves the already-issued Grant ID in
the unresolved Execution payload. The versioned Release Workflow records the
step as `UNKNOWN`/Run `BLOCKED`, keeps the fenced scheduler reservation, and
continues as an open Temporal Workflow. Every five minutes a read-only Activity
loads the same Grant dispatch and checks its Run, Step, Plan hash, target,
capability, artifact digest and idempotency key. Rollback recovery also matches
the Grant's external execution ID to the durable successful deploy. It never
issues a new Grant or calls a provider Write.

A verified terminal success updates the existing Execution with the complete
typed result, then resumes verification and normal Run completion. A rejected
result escalates the Run. While unresolved, an operator cancel marks the Run
cancelled but preserves the UNKNOWN step and scheduler reservation. A cancelled
or otherwise terminal Run cannot be advanced by the recovery Activity. Legacy or
non-Grant executions without a durable Grant ID remain blocked for operator
reconciliation.

This adds one versioned Temporal branch (`persistent-runner-recovery-wait`) and
the `RecoverExecution` Activity. Histories without that marker retain the prior
terminal BLOCKED behavior; completed Activity results are replayed unchanged.
The new path does not repair already-completed Runs or legacy records without a
Grant ID. Inventory those separately before upgrade.
Downgrading Workers would again return incomplete retry results, so a downgrade
does not preserve the new recovery guarantee.

## Verification scope

Issue #49 regression `TestDeploymentRetryPreservesAllFields` fails against the
old main Activity code and passes with this change. Memory tests inject response
loss after committed success, construct replacement Activities, restore both
deploy and rollback, pass both into verification, and count one Write per
operation. Negative tests cover legacy/missing payload, UNKNOWN, unsupported
version and incorrect execution/run/service/version/external ID/timestamps.

`TestPostgresDeploymentSuccessResponseLoss` does the same through real JSONB
storage and new database connections. It is included in `make test-integration`;
that target must only use a disposable environment because existing integration
tests can run down migrations and stop their database service. For just this
test, provide a dedicated disposable database explicitly:

```bash
THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB?sslmode=disable' \
  go test -tags=integration ./internal/workflow -run '^TestPostgresDeploymentSuccessResponseLoss$' -count=1
```

`TestPostgresUnknownGrantRecoveryAfterWorkerReplacement` stores the unresolved
Execution and Grant in PostgreSQL, reconnects with replacement Activities,
applies a terminal read-only Grant recovery and restores the typed Deployment
without another Write. `TestReleaseWorkflowWaitsForReadOnlyGrantRecovery` and
`TestCancelDuringUnknownRecoveryKeepsStepAndReservationUnresolved` exercise the
versioned Workflow state transitions in Temporal's deterministic test suite.
They do not yet constitute a live Temporal-server restart test for this new
UNKNOWN path.

`TestRestoredDeploymentIssuesBoundRollbackGrant` connects the restored result to
the real GrantExecutor and asserts rollback Run, service, external source ID and
idempotency key. Runner results in that test are simulated: it does not prove
Runner final policy acceptance or real AWS rollback. Those remain #48/#11.

`TestTemporalDeploymentCompletionLossAcrossWorkers` additionally uses real
PostgreSQL and Temporal. A test-only Activity wrapper suppresses completion with
`activity.ErrResultPending` after success persistence. The first Worker stops,
the server times out the Activity, and a replacement Worker restores its result.
The test repeats this for Rollback with a third Worker, then verifies the restored
rollback result. It checks full result equality and one Write per operation.
This models a lost completion and Worker replacement; it is not an OS process
kill or a live provider failure. Existing Temporal replay tests cover unchanged
history structure. No result here is a production-readiness claim.

Provider Writes and verification evidence in these recovery tests are
simulated. Record current per-commit results in the handoff progress log; older
verification above applies only to its named commits and tests.

## Recovery verification regression scope

`TestRecoveredDeploymentStillRequiresVerification` feeds an already-verified
recovery result into the Workflow and checks that the exact restored Deployment
reaches verification. PASS permits completion; FAIL, MISSING and INCONCLUSIVE
escalate when rollback is manual-only; unavailable verification blocks. It uses
simulated Activity results, not live runtime observations or signature validation.

`TestLegacyUnknownHistoryBranchDoesNotScheduleRecovery` selects DefaultVersion
for both version markers and checks terminal BLOCKED/UNKNOWN with no recovery
Activity. This is branch coverage in the Temporal test suite, **not** a replay of
recorded server history. Real old-history replay and live UNKNOWN recovery across
Worker replacement remain required before claiming upgrade compatibility.

`TestRecoveredRollbackRequiresIndependentVerification` covers pending then ready,
all four verification outcomes, unavailable verification, rejected recovery,
cancel while pending and the legacy version branch. It checks the rollback
recovery binding, exact restored Deployment passed to verification, durable
Run/Step outcomes and one dispatch of each operation. PASS after rollback means
ROLLED_BACK/FAILED, never release success. These are simulated Activities in the
Temporal test suite, not a live provider or server restart test.

After persisting UNKNOWN, the Workflow must rebind its Step pointer to the Run
returned by persistence before applying a recovered rejection. Persistence
returns a fresh snapshot; changing the previous snapshot would otherwise leave
the saved Step UNKNOWN while the Run becomes ESCALATED. Both deploy and rollback
rejections now persist ESCALATED consistently. This pointer correction adds no
Temporal commands or marker versions; historical Activity results stay intact.

`TestTemporalUnknownWaitCancelAcrossWorkersAndReplay` uses a dedicated real
PostgreSQL and Temporal server. For both deploy and rollback UNKNOWN, it waits
for the recovery Activity, replaces the Worker and database connection, sends
cancel, and checks CANCELLED/UNKNOWN in both Workflow result and PostgreSQL.
The complete fetched server history must replay with the current Workflow.
Provider/Grant-recovery results and the in-process coordinator in this fixture
are simulated. This proves current-history replay and cancellation across Worker
instances in one test process; it does not prove old-history upgrade replay,
OS process crash recovery, successful signed Grant convergence, or #50's shared
coordinator acceptance. Run only with explicit disposable environment addresses:

```bash
THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB?sslmode=disable' \
THEMISY_TEMPORAL_ADDRESS='DISPOSABLE_TEMPORAL:7233' \
  go test -tags=integration ./internal/workflow \
    -run '^TestTemporalUnknownWaitCancelAcrossWorkersAndReplay$' -count=1
```

Without either address this targeted test skips; a skip is not a passing live
integration result. Never point it at an existing shared database or namespace.

`TestTemporalSignedGrantRecoveryAcrossWorkers` connects actual Grant issuance,
GrantExecutor result waiting, signed Runner recovery over the existing HTTP
endpoint, fresh file-backed result trust, PostgreSQL Execution and shared
scheduler, and the live Temporal Workflow. A test-only short initial result wait
records the issued Grant as unresolved; the first recovery Activity loses its
completion. A replacement Worker retries that read-only Activity after the real
one-minute server timeout. An untrusted signature is rejected without advancing
the Grant, while the trusted report restores the same Execution, resumes Verify
and persists success. The test checks one Grant issuance, no replacement Write
and replay of the resulting success history. It needs the same two explicit
disposable environment variables as the cancel test and takes about a minute.
The provider, identity/delegation inputs and verification observations are
synthetic. This is not live AWS, a production trust-chain proof, or OS process
kill coverage. Rollback success through this complete signed path remains a
separate acceptance test.

`TestMainUnknownHistoriesReplay` replays two server histories produced by the
exact old main revision before either version marker existed. Their provenance,
old outcomes, normalized infrastructure metadata and regeneration procedure are
in [testdata](../../internal/workflow/testdata/README.md). This covers those two
legacy error paths, not every old approval/timer/cancel history.

`TestTemporalSignedRollbackRecoveryAcrossWorkers` additionally supplies the
initial deploy's signed success, observes failed deploy verification, issues one
rollback Grant bound to that saved deployment, then loses the rollback recovery
completion and replaces the Worker. Signed recovery restores that same rollback
Execution; independent rollback verification passes and the Run ends
ROLLED_BACK/FAILED. It asserts one deploy Grant, one rollback Grant and zero
replacement Writes. Both signed tests use distinct tenant scopes and the actual
PostgreSQL coordinator; synthetic observations still do not prove live runtime
health or Runner final authorization.

`TestTemporalSignedRecoveryAcrossProcesses` runs both signed recovery paths in
separate Worker OS processes. The first process is forcibly killed after the
read-only recovery Activity begins without completing. A fresh process with a
new store and write-forbidding executor resumes after server timeout. It must
converge the same Execution, preserve signed Grant/source bindings and replay the
complete history. The replacement process checks zero Write calls before exit.
Grant issuance is counted from durable `grant.issue` audits, since claiming a
delivery consumes its pending outbox entry. Synthetic provider/health and
identity inputs remain outside the live AWS/production identity gate.
