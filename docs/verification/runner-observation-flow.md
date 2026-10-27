# Standard Runner observation flow

The standard Control Plane `GrantExecutor.Verify` now derives a read-only
`release.verify` request from the exact persisted successful deploy/rollback
Grant and the pinned observation window. Runner verifies the source in its
journal, obtains only verify-purpose credentials and collects ECS runtime plus
Datadog health. Signed results, raw runtime, metric Evidence and combined
Evidence are persisted through the existing authenticated report endpoint.
The Workflow waits for this result; dispatch acceptance is never PASS.

The Run API exposes observation/source Grant IDs and runtime facts on each
verification. The Run UI shows status/reason, source, query, threshold, measured
value, sample/collection window and runtime confirmation; the audit contains
full signed reports and key IDs. Both deploy and rollback use the same checks.

## Configure and migrate

Follow [ADR-0024](../adr/0024-source-bound-runtime-verification.md). Configure all
ECS targets with distinct deploy/rollback/read-only verify roles, `release.verify`
capability, matching trusted Datadog query and a mode-0600 credential file.
Roles/queries/provider IDs are Runner-owned; requests cannot replace them.
Upgrade both endpoints before enabling the capability. Preserve old manifests,
nonces, idempotency keys and UNKNOWN reservations. Existing successful results
without a source Grant binding require operator handling, never a second Write.

A source Plan or identity may expire while an already-authorized operation is
observed; the read request creates no new Write permission. Read request expiry,
missing source, UNKNOWN source, bad signature, missing query, missing metrics,
stale metrics and unavailable runtime do not become PASS. Full compensation
lifetimes remain #51. Other Provider runtime verification is not implemented;
production startup refuses that configuration.

## Reproduce verification

- `go test -race ./internal/runner ./internal/resultreport ./internal/application`
  checks four-value classification/replay, source/target/window mismatches before
  credentials, expired-forward-identity observation, raw-body/signature/source
  tampering and the signed HTTP deploy → metrics FAIL → rollback → PASS flow.
  This flow uses the actual Datadog adapter with an HTTP fixture; its runtime
  Provider and identity source are synthetic.
- `TestWorkflowConsumesOnlyAuthenticatedObservationAndReverifiesRollback` runs
  the real Workflow/Activities in Temporal's test environment with signed HTTP
  delivery. Valid FAIL triggers rollback and authenticated re-verification;
  a deliberately corrupted observation signature leaves the Workflow BLOCKED,
  with one source Write and no successful verification projection.
- `make test-signed-observation` creates and removes only its owned temporary
  PostgreSQL container. It runs the same signed flow with separate Control Plane
  and Runner schemas, closes/reopens pools and revalidates stored signatures,
  bodies/hashes, query and sample time. No caller database URL is used.
- ECS adapter runtime tests separately check real reader code against SDK
  fixtures, including missing/wrong tasks, digests, health and rollout.
- `make test-workflow-recovery` covers real PostgreSQL/Temporal Worker crashes,
  signed deploy/rollback recovery and expired authority, with simulated Providers.

CI requires the dedicated signed-observation target. No live AWS or Datadog,
production identity/directory, or staging deployment is asserted by these tests.
