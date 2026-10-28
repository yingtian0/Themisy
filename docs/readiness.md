# Readiness and supported evaluation scope

Reviewed 2026-10-28 (Asia/Tokyo). This is a development snapshot, not a release
or a production support commitment. Task order: [#52](https://github.com/yingtian0/Themisy/issues/52).

## Versions and evidence

| Snapshot | Evidence and meaning |
|---|---|
| main `8eb8a400d4df468036e101927c177cbd66a5d723` | [CI run 38865188601](https://github.com/yingtian0/Themisy/actions/runs/38865188601), all five jobs succeeded; includes real PostgreSQL/Temporal recovery with simulated provider boundaries |
| offline course `671fe6953a09b2156ec1542cce877e798f18e8c7` | [PR #53](https://github.com/yingtian0/Themisy/pull/53), [local verification](guides/offline-verification.md); [CI run 39090979789](https://github.com/yingtian0/Themisy/actions/runs/39090979789), all five jobs succeeded on this commit |
| main `8808bf733cb57e75d9c600f2f33ad1b2c0fcde82` | [CI run 40090297722](https://github.com/yingtian0/Themisy/actions/runs/40090297722), all five jobs succeeded; current integration and source-observation baseline |
| coordinator acceptance `1d9fbfbd2a65d821c625278f228698eb108cd5aa` | Dedicated PostgreSQL independent-store and independent-OS-process tests passed locally with race detection; [reproduction target](runbooks/workflow-coordinator.md#reproduce-the-acceptance-gate) is now required in CI |
| runtime verification `dc5f584fa4ac7f145639f81d7a512ed8f5509309` | [PR #93](https://github.com/yingtian0/Themisy/pull/93), [CI 40452048603](https://github.com/yingtian0/Themisy/actions/runs/40452048603), all five jobs succeeded; signed read-only observations, real PostgreSQL fixtures and simulated provider boundaries |
| production identity `8d94cd97020f98fea6f66a442eb944dc516eed3d` | [PR #94](https://github.com/yingtian0/Themisy/pull/94), [CI 40624014693](https://github.com/yingtian0/Themisy/actions/runs/40624014693), all five jobs succeeded; independent identity attestation and durable Directory fences; no live IdP/cloud evidence |
| Policy distribution `8724513cf2897a0e3a8a849d73b7af5a959f8489` | [PR #95](https://github.com/yingtian0/Themisy/pull/95), [CI 40628517563](https://github.com/yingtian0/Themisy/actions/runs/40628517563), all five jobs succeeded; scoped rollout, durable revision fences and synthetic customer boundaries |
| This documentation revision | Git commit adding this table; document/link checks recorded in the associated PR. No execution changes |

“Implemented” means code exists. “Simulated” includes mocks, fake SDKs, memory
stores and the Temporal test environment. “Real DB/Temporal” refers only to the
integration scenarios in CI, not a live cloud or every possible crash boundary.
No dedicated live AWS or independent user trial evidence is recorded here.
Skipped, missing and inconclusive checks are not passes.

## Evaluation scope

| Scope | Enabled for evaluation | Limits / configuration |
|---|---|---|
| Offline planning | CLI validation, deterministic Plan/hash, explicit rejection | Source-built CLI; local files, synthetic Context. No identity/evidence authentication, approval, Grant or Write. Locally checked on macOS arm64 Go 1.26.9; Linux checks via CI; Windows unverified |
| Local execution development | Explicitly assembled Control Plane/Worker/Runner and dedicated DB/Temporal | Not a turnkey course. Example secrets/trust/identity/manifest material are not bundled; no default mock Write guarantee. See README prerequisites |
| Limited ECS staging | Implementation available to developers configuring one isolated staging service | Live deploy/reconcile/rollback and IAM bypass-denial gate unverified. Do not advertise as a validated course. No production credentials |
| Production | No supported configuration declared | Separate gates below; passing package or planning tests cannot enable this scope |

This table documents restrictions; it does not add feature flags to disable the
execution binaries. Only `offline-plan` is the credential-free evaluation path.
The standard persistent Store now supplies a shared PostgreSQL coordinator.
Multiple workers must use the same database and identical scheduling limits;
follow the [migration/restart procedure](runbooks/workflow-coordinator.md) and
pass `make test-workflow-coordinator` before evaluating that configuration.
That target verifies two OS processes, independent pools, UNKNOWN retention,
expired ownership fencing and circuit persistence. In-memory/custom stores do
not gain these guarantees. A single task queue alone provides no exclusion.
Unresolved operations keep capacity reserved; restarting does not authorize a
new Write. [ADR-0012](adr/0012-control-plane-workflow-coordinator.md) remains
Proposed; local acceptance is not production approval.

## Execution status and failure paths

Paths below are repository-relative references in this revision. Historical PRs
identify provenance; commit-specific CI evidence is listed above. New source-admission tests in this revision have local fixture evidence and require
the PR's exact-head CI checks before merge. None of these rows is live cloud evidence.

| Capability / scenario | Implementation / simulated evidence | Real DB / Temporal evidence | Real AWS / remaining gate |
|---|---|---|---|
| Contract/Profile validation, Plan/hash/expiry | `internal/contract`, `internal/profile`, `internal/planner`; offline tests compare full Plan | Not needed for offline planning | No cloud claim; [#41](https://github.com/yingtian0/Themisy/issues/41), [#51](https://github.com/yingtian0/Themisy/issues/51) |
| Signed Grant, approval refusal and tamper rejection | `internal/application/grants_test.go`, `internal/runner/runner_test.go`; rejects before credentials/Write. `internal/runner/final_policy_grant_test.go` connects approved deploy, simulated failed observation and separately evaluated rollback | Real PostgreSQL `TestPostgresFinalPolicyIssuanceAtomicity` verifies atomic Grant/audit and concurrent revocation refusal | [#4](https://github.com/yingtian0/Themisy/issues/4), final-input evidence [#48](https://github.com/yingtian0/Themisy/issues/48) / [migration](runbooks/final-operation-policy.md); production lifetime redesign remains #51 |
| Production verification cannot be omitted | [PR #36](https://github.com/yingtian0/Themisy/pull/36), `internal/workflow/release_test.go`; [migration](runbooks/production-verification.md) | Main CI includes integration suite; guard tests are simulated | Completed [#35](https://github.com/yingtian0/Themisy/issues/35) unit, distinct from parent #9 |
| ECS runtime health required before success | [PR #93](https://github.com/yingtian0/Themisy/pull/93), signed source-bound runtime/health reports and standard GrantExecutor connection; [migration](verification/runner-observation-flow.md) | `TestPostgresSignedObservationFlow` reopens durable pools and reverifies reports; Workflow fixture rejects forged observations | Local acceptance of #9 complete; live ECS/Datadog staging remains #11 |
| Missing/inconclusive Evidence | Four-value health/runtime results are preserved in signed reports; no missing result becomes PASS | Signed observation/audit and Workflow tests use fixtures | #9 local gate implemented; real provider outage/stale-data staging unverified |
| Rollback and subsequent verification | Typed separate operation and simulated executor tests exist | `internal/workflow/deployment_result_integration_test.go` and signed recovery tests cover complete restored Deployment and one Write; `internal/application/deployment_restore_test.go` binds rollback Grant to restored source execution. Dedicated PostgreSQL/Temporal recovery target includes Worker process crashes | [#49](https://github.com/yingtian0/Themisy/issues/49), #48, #51; live rollback unverified |
| UNKNOWN and restart, no blind retry | `internal/runner/recovery_integration_test.go`, journal ownership and read-only reconciliation | Main CI real PostgreSQL shared-journal/process recovery plus Temporal integration | Workflow/Run convergence [#38](https://github.com/yingtian0/Themisy/issues/38) and backup/retention [#30](https://github.com/yingtian0/Themisy/issues/30) completed. `make test-workflow-recovery` and `make verify-runner-journal-backup-restore` reproduce positive/negative gates; live AWS remains unverified |
| STS credentials and fixed ECS targets | SDK fakes in `pkg/credentials`, `adapters/ecs`; Runner-local allowlists plus active registered task-definition and exact digest preflight ([PR #86](https://github.com/yingtian0/Themisy/pull/86)) | Not a cloud credential test | [#6](https://github.com/yingtian0/Themisy/issues/6), [#7](https://github.com/yingtian0/Themisy/issues/7), [#11](https://github.com/yingtian0/Themisy/issues/11) live KMS/STS/ECS/CloudTrail and direct Write denial pending |
| Policy distribution | Authenticated registry/assignment/status/shadow/rollback API, scoped monotonic revisions, independent embedded mandatory baseline, fresh Runner verification; [guide](guides/policy-distribution.md) | Exact-head CI above includes real-PostgreSQL restart and concurrent admission tests plus the race suite | #10 local implementation merged; real customer canary rollout unverified |
| Identity | [PR #94](https://github.com/yingtian0/Themisy/pull/94): OIDC REST/UI/MCP, separate mTLS identity attestor, signed Directory, requester/approver/delegation rechecks; [guide](guides/production-identity.md) | Exact-head CI above includes real DB fences and simulated HTTPS IdP/exchange boundaries | #12 local trust chain implemented; customer key/cert/Directory provisioning and live IdP validation unverified |
| Pre-dispatch Context | [Signed source admission](guides/source-admission.md) connects independently signed build provenance, typed CI/dependency observations and verified metadata to standard Plan/Grant/Runner authorization | Local real-PostgreSQL source fences and end-to-end fixtures; exact-head PR CI required | #13 remains open: owner confirmed staging unavailable; [external procedure](guides/source-admission-staging.md) is unexecuted; caller booleans are not authority |

Private security triage is tracked through [SECURITY.md](../SECURITY.md), with
channel availability in [#39](https://github.com/yingtian0/Themisy/issues/39).
This public table contains no unpublished report details. Evaluate unresolved
reports privately before recommending any affected execution scope.

## Separate production gates

All require versioned evidence and review; there is no promised completion date:

- Production identity deployment validation and customer provisioning remain external gates. The local OIDC/Directory trust chain is implemented; trusted pre-dispatch Context freshness/provenance remains #13.
- Customer Policy provisioning and real canary/shadow/rollback rollout validation remain external gates. Local signed distribution and mandatory enforcement are implemented; long-lived operation/compensation authorization remains #51.
- Key custody, rotation and revocation for Grants, manifests and results (#4).
- Real runtime health and signed Evidence, including unavailable and missing
  observations, deploy failure, rollback and rollback verification (#9).
- Restart/response-loss recovery across Runner, Control Plane and Temporal,
  durable ownership/concurrency and verified backup/restore retention (#30/#38/#49/#50).
- Dedicated AWS staging proves permitted KMS/STS/ECS operations and denies
  direct writes from normal CI/Agent identities (#6/#7/#11). Runner trust and
  absence of alternate CI Write permissions remain explicit assumptions.

## Release review checklist

Every release PR must update this table and its known limits:

- Pin source commit/tag and immutable CI/evidence links; state simulated,
  real DB/Temporal, live AWS and user trials separately.
- Record supported OS/configurations, enabled evaluation scope, restrictions,
  migrations, unresolved Issues and private security review status without details.
- Include failure/recovery results alongside normal results. Explain any skip.
- Verify distribution checksum/SBOM/provenance and reproduce the offline guide
  from the actual package on a clean machine ([#43](https://github.com/yingtian0/Themisy/issues/43)).
- Record external observations and continued use separately ([#47](https://github.com/yingtian0/Themisy/issues/47)).
  Use the [trial protocol](evaluation/external-trial-protocol.md) and
  [actual-results ledger](evaluation/results.md); prepared questions are not observations.

Checks absent from this table and a small trial with zero bad writes do not prove
safety or production support. Release publication remains a separate decision.

## 2026-10-27 acceptance review

The [offline verification record](guides/offline-verification.md#revalidation-on-2026-10-27)
records the English/Japanese normal and refused examples, CLI help, isolated
cycle, field mapping and artifact exercise on the pinned main above. Full
repository race/shuffle tests, integration-tag vet and build passed on macOS
arm64, Go 1.26.10. The independent 15-minute participant trial is unmeasured.

Dedicated local PostgreSQL reproduced final-policy atomicity; the coordinator
acceptance revision separately reproduced contention, UNKNOWN retention after
lease expiry, persistent open circuit and two independent OS processes. These
are synthetic local provider boundaries, not live AWS. CI includes the
coordinator target explicitly so its opt-in tests cannot silently disappear
from the normal gate. Failed/skipped or externally unavailable checks are not
completion evidence. No distribution was published and no live AWS operation
was performed by this review.

The [Issue acceptance ledger](verification/issue-acceptance-2026-10-27.md) maps
the ten bounded completion scopes to their checks and lists the older gates
that remain open.

## Source-bound verification connection

The implementation accompanying [ADR-0024](adr/0024-source-bound-runtime-verification.md)
connects the standard GrantExecutor to read-only Runner verification for ECS,
including signed runtime/health bodies and tenant-scoped API/UI projection.
The earlier snapshot's unavailable-executor statement is superseded by that implementation.
See [reproduction and migration](verification/runner-observation-flow.md) for
required read roles, queries, credentials and fail-closed behavior. This does not
complete live AWS/Datadog staging, production identity/Context or compensation
lifecycle gates. Updated exact-head CI evidence is recorded in the change PR.
