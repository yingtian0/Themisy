# Readiness and supported evaluation scope

Reviewed 2026-10-18 (Asia/Tokyo). This is a development snapshot, not a release
or a production support commitment. Task order: [#52](https://github.com/yingtian0/Themisy/issues/52).

## Versions and evidence

| Snapshot | Evidence and meaning |
|---|---|
| main `8eb8a400d4df468036e101927c177cbd66a5d723` | [CI run 38865188601](https://github.com/yingtian0/Themisy/actions/runs/38865188601), all five jobs succeeded; includes real PostgreSQL/Temporal recovery with simulated provider boundaries |
| offline course `671fe6953a09b2156ec1542cce877e798f18e8c7` | [PR #53](https://github.com/yingtian0/Themisy/pull/53), [local verification](guides/offline-verification.md); [CI run 39090979789](https://github.com/yingtian0/Themisy/actions/runs/39090979789), all five jobs succeeded on this commit |
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
One Worker process is a development limitation: service reservations and circuit
state are process-local and do not survive restart as global controls. A single
task queue alone does not enforce cross-worker exclusion. Do not scale Workers
or treat restart as permission to dispatch unresolved operations ([#50](https://github.com/yingtian0/Themisy/issues/50)).

## Execution status and failure paths

Paths below are repository-relative evidence references at the main snapshot
above, browsable in the [pinned source tree](https://github.com/yingtian0/Themisy/tree/8eb8a400d4df468036e101927c177cbd66a5d723).
Later implementations must provide new commit-specific evidence.

| Capability / scenario | Implementation / simulated evidence | Real DB / Temporal evidence | Real AWS / remaining gate |
|---|---|---|---|
| Contract/Profile validation, Plan/hash/expiry | `internal/contract`, `internal/profile`, `internal/planner`; offline tests compare full Plan | Not needed for offline planning | No cloud claim; [#41](https://github.com/yingtian0/Themisy/issues/41), [#51](https://github.com/yingtian0/Themisy/issues/51) |
| Signed Grant, approval refusal and tamper rejection | `internal/application/grants_test.go`, `internal/runner/runner_test.go`; rejects before credentials/Write | CI integration covers persisted application/Runner paths, not complete approval lifecycle | [#4](https://github.com/yingtian0/Themisy/issues/4), final approved/rollback Policy input [#48](https://github.com/yingtian0/Themisy/issues/48) |
| Production verification cannot be omitted | [PR #36](https://github.com/yingtian0/Themisy/pull/36), `internal/workflow/release_test.go`; [migration](runbooks/production-verification.md) | Main CI includes integration suite; guard tests are simulated | Completed [#35](https://github.com/yingtian0/Themisy/issues/35) unit, distinct from parent #9 |
| ECS runtime health required before success | [PR #37](https://github.com/yingtian0/Themisy/pull/37), `adapters/ecs`, `internal/runner/dispatch_reconcile_test.go` | Shared journal tests preserve dispatch/reconcile state | Runtime gate unit implemented. Full Evidence body/storage/API/UI and end-to-end verification remain [#9](https://github.com/yingtian0/Themisy/issues/9); default Control Plane verification executor unavailable |
| Missing/inconclusive Evidence | `internal/verification`, `adapters/datadog` four-value tests; unavailable workflow verifier blocks | No complete real-backend evidence course recorded | Never mapped to PASS; parent #9 |
| Rollback and subsequent verification | Typed separate operation and simulated executor tests exist | No full success-response-loss → restored Deployment → rollback Grant proof recorded | [#49](https://github.com/yingtian0/Themisy/issues/49), #48, #51; live rollback unverified |
| UNKNOWN and restart, no blind retry | `internal/runner/recovery_integration_test.go`, journal ownership and read-only reconciliation | Main CI real PostgreSQL shared-journal/process recovery plus Temporal integration | Runner recovery unit exists; Run/workflow convergence remains [#38](https://github.com/yingtian0/Themisy/issues/38); backup/retention [#30](https://github.com/yingtian0/Themisy/issues/30) remains open |
| STS credentials and fixed ECS targets | SDK fakes in `pkg/credentials`, `adapters/ecs`; Runner-local allowlists | Not a cloud credential test | [#6](https://github.com/yingtian0/Themisy/issues/6), [#7](https://github.com/yingtian0/Themisy/issues/7), [#11](https://github.com/yingtian0/Themisy/issues/11) live KMS/STS/ECS/CloudTrail and direct Write denial pending |
| Policy, identity and pre-dispatch Context | Signed bundle/OIDC/delegation primitives exist; local examples are synthetic | No production trust-chain deployment evidence | [#10](https://github.com/yingtian0/Themisy/issues/10), [#12](https://github.com/yingtian0/Themisy/issues/12), [#13](https://github.com/yingtian0/Themisy/issues/13); do not infer production readiness |

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
