# Production verification requirement

Production Plans require `spec.verification.required: true` in every selected
Release Profile. Omission decodes as false and is rejected, as is explicit false.
Staging may explicitly disable verification. The legacy service-only planner
continues to generate verification-required steps.

The worker validates all Production Plan/run step mappings and verification
flags at the existing EvaluateStep Activity before dispatch. A later unsafe
step therefore blocks the first deployment too. Missing/duplicate mappings fail
closed. Deploy and Rollback recheck the durable run before reserving an execution,
including Activities queued by an older worker. PersistRun rejects environment
identity changes, replacement of Production Plan/verification step configuration,
and unsafe successful run/step projections. Errors loading the
durable run also stop these operations. Existing Workflow commands are unchanged;
there is no new Workflow branch or Temporal version marker.

## Migration

Before upgrading workers, freeze the affected Runner groups and pause unsafe
Production runs. Inventory their pinned Profiles, Plans, approvals, execution
journals, and all pre-issued or leased Action Grants, including in-flight and
UNKNOWN executions. Drain old workers: a worker running old Activity code does
not provide these guards. Pausing a Workflow or replacing its worker does not
recall a previously signed Grant or stop a write already in flight; independently
leased Grants may still reach a Runner.

Keep the affected Runner groups frozen until outstanding unsafe Grants have
expired or have been fenced through supported manifest revision/revocation
controls. Follow the [Runner recovery and manifest runbook](runner-recovery-and-manifests.md)
for revision fencing and read-only reconciliation of existing writes. Reconcile
in-flight and UNKNOWN executions before unfreezing; expiry or lease loss does
not authorize another write. Use supported operator controls, without direct
database edits or rewriting Temporal history.

Update the source Profiles to explicitly require verification, then generate new
Plans and obtain new approvals bound to those hashes. Do not edit flags in an
old pinned Plan or reuse its approval. Cancel unsafe pending runs and submit the
regenerated release after reconciling any already dispatched/unknown executions.
Use the established read-only reconciliation/operator recovery process for
external writes already in flight; this change does not undo them or authorize
retries. A queued Activity may now fail and require operator recovery rather than
continue an old unsafe release.

Completed Activity results in Temporal history are replayed without reexecuting
the Activity, and completed releases are not rewritten by this change. New success
persistence is guarded, but historical unsafe successes must be audited separately.
These tests use the in-memory store, Temporal test environment, and Mock executor.
This guard is not real runtime observation or signed PASS evidence; that work
remains part of issue #9.
