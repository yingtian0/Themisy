# Issue acceptance review: 2026-10-27

Baseline: main [`8808bf733cb57e75d9c600f2f33ad1b2c0fcde82`](https://github.com/yingtian0/Themisy/tree/8808bf733cb57e75d9c600f2f33ad1b2c0fcde82).
[CI 40090297722](https://github.com/yingtian0/Themisy/actions/runs/40090297722)
passed all five jobs. This review verifies completed scopes, fixes a missing
coordinator CI gate and updates stale documentation. It does not manufacture
new feature implementations for already-integrated work.

## Bounded completion scopes

| Issue | Acceptance evidence and limits |
|---|---|
| [#41](https://github.com/yingtian0/Themisy/issues/41) Offline course | Normal/refused built-CLI examples, deterministic complete Plan equality against core planner, rejecting HTTP transport guard, explicit synthetic/no-authority output, README entry and independent-trial record fields. See [dated verification](../guides/offline-verification.md#revalidation-on-2026-10-27). No participant timing claim. |
| [#42](https://github.com/yingtian0/Themisy/issues/42) Readiness matrix | [Current matrix](../readiness.md) separates implementation, simulated providers, real PostgreSQL/Temporal, live AWS and unmeasured user trials; fixed commits/CI, failure paths, supported scope, remaining production gates and release checklist. |
| [#48](https://github.com/yingtian0/Themisy/issues/48) Final Policy inputs | `TestFinalApprovedDeployAndRollbackReachRunner` uses actual Grant issuance, mandatory Policy and Runner code: initial REQUIRE_APPROVAL, approval, deploy, failed observation, independent rollback hash, tampered approval/capability/artifact/Context/input refused before credentials/Write, duplicate delivery no second Write. `TestFinalPolicyRefusesStaleAuthorizationAndFacts` covers expired/revoked approval, changed Policy, missing facts and rollback-specific authorization. Real PostgreSQL `TestPostgresFinalPolicyIssuanceAtomicity` passed locally for atomic persistence and concurrent approval revocation. No Workflow command graph change. [Migration](../runbooks/final-operation-policy.md); #51 retains the distinct lifetime redesign. |
| [#49](https://github.com/yingtian0/Themisy/issues/49) Complete Deployment restoration | `TestPostgresDeploymentSuccessResponseLoss` covers deploy/rollback success persistence, connection replacement, all required fields, subsequent verification and one Write each. `TestRestoredDeploymentIssuesBoundRollbackGrant` binds restored Run/service/source execution/idempotency to actual rollback Grant issuance. Memory negative tests reject legacy/incomplete/conflicting results. Pinned CI runs `make test-workflow-recovery`: real Temporal/PostgreSQL, Worker process crashes and signed deploy/rollback recovery. Providers are simulated, not AWS. |
| [#50](https://github.com/yingtian0/Themisy/issues/50) Shared coordinator | Existing shared PostgreSQL reservation/admission/fencing/audit implementation plus required `make test-workflow-coordinator` CI gate. Local race tests passed for independent stores, two OS processes contending for one budget, duplicate/conflicting completion, UNKNOWN capacity after lease expiry, stale fence/expired admission refusal, and persisted open breaker after Store restart. Workflow fail-closed tests refuse unavailable coordinator/missing lease. [Operations](../runbooks/workflow-coordinator.md). ADR-0012 remains Proposed; this is not production deployment approval. |
| [#58](https://github.com/yingtian0/Themisy/issues/58) Japanese course | Japanese/English commands and safety notices agree; normal/refused/help/cycle commands reproduced with built CLI, relative links exist, independent timing fields remain unmeasured. |
| [#59](https://github.com/yingtian0/Themisy/issues/59) Field explanation | Generated service/artifact/profile/runner-group values match [field table](../../examples/offline/README.md); hash origins and synthetic authority limits remain explicit. Go prerequisite now matches `go.mod`. No schema change. |
| [#60](https://github.com/yingtian0/Themisy/issues/60) Discoverable help | Actual `help`, `help offline-plan`, `offline-plan -h` exit 0; surplus topic arguments exit 1. CLI regression tests include existing HTTP routing and offline network refusal. |
| [#61](https://github.com/yingtian0/Themisy/issues/61) Isolated cycle | Built CLI with isolated fixture exits 1 / `DEPENDENCY_CYCLE` / empty stdout; normal course still exits 0. Existing planner and CLI tests pass without execution components. |
| [#62](https://github.com/yingtian0/Themisy/issues/62) Artifact exercise | Temporary copies, same fixed `-at`: desired version/Change/Plan hashes differ; Contract/Profile hashes and timestamps equal; tracked sample unchanged; guide links exist and authority limitations preserved. |

The full baseline `go test -race -shuffle=on ./...`, integration-tag vet and
build passed locally on macOS arm64 / Go 1.26.10. New coordinator checks passed
against a container created solely for the test, with no caller database URL.
The owning script removes only its own container/volumes. A busy required port
fails safely rather than connecting to an existing database.

## Older gates retained

Issues were inspected in creation order. #6/#7 still require actual STS/ECS
staging evidence; #9 needs complete signed Evidence/application/UI integration;
#10 needs real Policy distribution/activation/rollback; #11 needs actual AWS
and bypass-denial proof; #12 needs the production identity/delegation trust
chain; #13 needs default authorization wiring and real-source staging evidence.
Existing opt-in collectors and observations do not complete these parents.
#39 requires an operational private reporting channel. #40 still requires its
complete README command/configuration audit. They are not closed merely to
reach a numerical target. This review creates no live AWS resources, reports no
external user observations and publishes no release. Existing #30/#38 closures
from the preceding batch are not counted among these ten Issues.
