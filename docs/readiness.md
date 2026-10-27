# Readiness and supported evaluation scope

Reviewed 2026-10-27 (Asia/Tokyo). This is a development snapshot, not a release
or a production support commitment. Task order: [#52](https://github.com/yingtian0/Themisy/issues/52).

## Versions and evidence

| Snapshot | Evidence and meaning |
|---|---|
| main `8eb8a400d4df468036e101927c177cbd66a5d723` | [CI run 38865188601](https://github.com/yingtian0/Themisy/actions/runs/38865188601), all five jobs succeeded; includes real PostgreSQL/Temporal recovery with simulated provider boundaries |
| offline course `671fe6953a09b2156ec1542cce877e798f18e8c7` | [PR #53](https://github.com/yingtian0/Themisy/pull/53), [local verification](guides/offline-verification.md); [CI run 39090979789](https://github.com/yingtian0/Themisy/actions/runs/39090979789), all five jobs succeeded on this commit |
| main `8808bf733cb57e75d9c600f2f33ad1b2c0fcde82` | [CI run 40090297722](https://github.com/yingtian0/Themisy/actions/runs/40090297722), all five jobs succeeded; current integration and source-observation baseline |
| coordinator acceptance `1d9fbfbd2a65d821c625278f228698eb108cd5aa` | Dedicated PostgreSQL independent-store and independent-OS-process tests passed locally with race detection; [reproduction target](runbooks/workflow-coordinator.md#reproduce-the-acceptance-gate) is now required in CI |
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

Paths below are repository-relative evidence references at the main snapshot
above, browsable in the [pinned source tree](https://github.com/yingtian0/Themisy/tree/8808bf733cb57e75d9c600f2f33ad1b2c0fcde82).
Later implementations must provide new commit-specific evidence.

| Capability / scenario | Implementation / simulated evidence | Real DB / Temporal evidence | Real AWS / remaining gate |
|---|---|---|---|
| Contract/Profile validation, Plan/hash/expiry | `internal/contract`, `internal/profile`, `internal/planner`; offline tests compare full Plan | Not needed for offline planning | No cloud claim; [#41](https://github.com/yingtian0/Themisy/issues/41), [#51](https://github.com/yingtian0/Themisy/issues/51) |
| Signed Grant, approval refusal and tamper rejection | `internal/application/grants_test.go`, `internal/runner/runner_test.go`; rejects before credentials/Write. `internal/runner/final_policy_grant_test.go` connects approved deploy, simulated failed observation and separately evaluated rollback | Real PostgreSQL `TestPostgresFinalPolicyIssuanceAtomicity` verifies atomic Grant/audit and concurrent revocation refusal | [#4](https://github.com/yingtian0/Themisy/issues/4), final-input evidence [#48](https://github.com/yingtian0/Themisy/issues/48) / [migration](runbooks/final-operation-policy.md); production lifetime redesign remains #51 |
| Production verification cannot be omitted | [PR #36](https://github.com/yingtian0/Themisy/pull/36), `internal/workflow/release_test.go`; [migration](runbooks/production-verification.md) | Main CI includes integration suite; guard tests are simulated | Completed [#35](https://github.com/yingtian0/Themisy/issues/35) unit, distinct from parent #9 |
| ECS runtime health required before success | [PR #37](https://github.com/yingtian0/Themisy/pull/37), `adapters/ecs`, `internal/runner/dispatch_reconcile_test.go` | Shared journal tests preserve dispatch/reconcile state | Runtime gate unit implemented. Full Evidence body/storage/API/UI and end-to-end verification remain [#9](https://github.com/yingtian0/Themisy/issues/9); [Opt-in read-only runtime snapshots](verification/runtime-observations.md) are audited and freshness checked; default Control Plane verification executor remains unavailable |
| Missing/inconclusive Evidence | `internal/verification`, `adapters/datadog` four-value tests; unavailable workflow verifier blocks | No complete real-backend evidence course recorded | Never mapped to PASS; parent #9 |
| Rollback and subsequent verification | Typed separate operation and simulated executor tests exist | `internal/workflow/deployment_result_integration_test.go` and signed recovery tests cover complete restored Deployment and one Write; `internal/application/deployment_restore_test.go` binds rollback Grant to restored source execution. Dedicated PostgreSQL/Temporal recovery target includes Worker process crashes | [#49](https://github.com/yingtian0/Themisy/issues/49), #48, #51; live rollback unverified |
| UNKNOWN and restart, no blind retry | `internal/runner/recovery_integration_test.go`, journal ownership and read-only reconciliation | Main CI real PostgreSQL shared-journal/process recovery plus Temporal integration | Workflow/Run convergence [#38](https://github.com/yingtian0/Themisy/issues/38) and backup/retention [#30](https://github.com/yingtian0/Themisy/issues/30) completed. `make test-workflow-recovery` and `make verify-runner-journal-backup-restore` reproduce positive/negative gates; live AWS remains unverified |
| STS credentials and fixed ECS targets | SDK fakes in `pkg/credentials`, `adapters/ecs`; Runner-local allowlists plus active registered task-definition and exact digest preflight ([PR #86](https://github.com/yingtian0/Themisy/pull/86)) | Not a cloud credential test | [#6](https://github.com/yingtian0/Themisy/issues/6), [#7](https://github.com/yingtian0/Themisy/issues/7), [#11](https://github.com/yingtian0/Themisy/issues/11) live KMS/STS/ECS/CloudTrail and direct Write denial pending |
| Policy, identity and pre-dispatch Context | Signed bundle/OIDC/delegation primitives, reverified local candidate inventory and audited shadow observations; delegation expiry rechecked after resolution. [Typed source assembly](context/source-context.md) combines collector-minted CI/dependency/metadata observations; local examples remain synthetic | Dedicated PostgreSQL audit tests cover source assembly, shadow and runtime observations. No production trust-chain deployment evidence | [#10](https://github.com/yingtian0/Themisy/issues/10), [#12](https://github.com/yingtian0/Themisy/issues/12), [#13](https://github.com/yingtian0/Themisy/issues/13); do not infer production readiness |

Private security triage is tracked through [SECURITY.md](../SECURITY.md), with
channel availability in [#39](https://github.com/yingtian0/Themisy/issues/39).
This public table contains no unpublished report details. Evaluate unresolved
reports privately before recommending any affected execution scope.

## Separate production gates

All require versioned evidence and review; there is no promised completion date:

- Production identity, short-lived delegation and organization roles, JWKS
  refresh/revocation; trusted Context freshness/provenance (#12/#13).
- Signed Policy distribution, mandatory enforcement, shadow/rollout/rollback and
  operation-specific final authorization (#10/#48/#51).
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
