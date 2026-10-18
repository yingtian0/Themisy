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
and read-only provider state under the existing operator process. Automatic
UNKNOWN-to-Run/workflow convergence remains Issue #38.

This changes Activity internals and durable payloads, not Workflow commands,
Activity names, or Activity result types. There is no command-graph change and
no new Temporal version marker. Already completed Activity results in Temporal
history are replayed unchanged; the new code does not repair old incomplete
history results or completed Runs. Inventory those separately before upgrade.
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

Local verification on 2026-10-18 (macOS arm64, Go 1.26.9): full ordinary package
tests, affected workflow/application/executor race/vet, all-package build, the
dedicated PostgreSQL reconnect test, and the real Temporal completion-loss test
passed. The latter used PostgreSQL 17.6 and Temporal 1.29.6.1, isolated from
existing services. First/second Worker replacement and both server-driven
Activity retries completed in approximately six seconds. Provider Writes and
verification evidence in these recovery tests are simulated.
