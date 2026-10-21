# Integration checkpoint — 2026-10-21

The maintainer explicitly requested merging all outstanding work. PR #72 was
merged first, retaining Go1.26.10. The remaining published feature branches are
combined below through merge commits; superseded test implementations retain
the later recovery and scheduling fixes. No source worktree changes were
stashed, discarded or bulk committed. The local verification branch contained
only already represented automatic merge commits and was retained in history.

## Included branch heads

| Branch | Source HEAD |
|---|---|
| codex/issue-10-policy-shadow | 5a3541d258f5b90349ad21f6060b97d7d7cb2848 |
| codex/issue-12-oidc-read-api | 966ef872782c284b13cafde3f2b9dcce139c44d8 |
| codex/issue-13-ci-source | 4d7b7063cf7355d7690da0fee7fc68767dc27c72 |
| codex/issue-30-backup-retention | 38b8c9b0b664f5d61ee490c0de5c62122f016401 |
| codex/issue-38-unknown-recovery | 5987d412c8655116216647edfa7173be347b73bb |
| codex/issue-38-workflow-resume | 6e7e713d5435aa37c7e250c9621362afbee19da7 |
| codex/issue-41-offline-plan | 671fe6953a09b2156ec1542cce877e798f18e8c7 |
| codex/issue-43-offline-package | eba33d0a001b41b710067d967c1c9c959b43805e |
| codex/issue-45-contributor-entry | b4c5b045293de2b9d2ce1f1fd435bba7ced021b8 |
| codex/issue-46-approval-context | e0768cae592d5ea5197b9fc29212015cda76fb13 |
| codex/issue-47-evaluation-protocol | 32f96b4d8b5d124177d660a0b1fb13814ca1a26e |
| codex/issue-48-final-policy-input | 9469b97b01783730f365c156aa946668e654f9c5 |
| codex/issue-49-deployment-restore | 2d1b11657a5097d5b8adb169503388ae7c2e99b7 |
| codex/issue-50-persistent-controls | 7487a65634145754c23b232374d5fcccf812f1d3 |
| codex/issue-50-worker-operations | eae281c691edae0a5b631d599a8be8476765ce9e |
| codex/issue-51-lifecycle-design | 1f927d472555b06cbb6b6adae74ae37d352e6b43 |
| codex/issue-58-japanese-offline-course | e38e74efdba0917ee9aca12bbdb6b39a8eb044d3 |
| codex/issue-59-offline-fields | 634f52441d39a7b1f2a7bf0482b59651da6e73b8 |
| codex/issue-6-localstack-sts | c482fc7cd884083fa76934351ae9f710df169a76 |
| codex/issue-60-offline-help | bb4bc664ad864c51320eeeff1c548cfd385554cc |
| codex/issue-61-offline-cycle | 5b6d48c8d6d6d58b299482357e8ee4cc21998c0c |
| codex/issue-62-offline-editing | a6ceefe8b82cc8411e6635ab44f4ee5cff9a92a2 |
| codex/issue-9-signed-evidence-result | 2bdf14d24a4371bb8b20172017f686aa105b785f |
| codex/issues-40-42-readiness | 12e58f2ea5d80295f90b043fa30d9c1c0f9a8109 |

## Integration resolutions

- Preserve all sixteen ADR entries and their recorded status. Proposed ADRs are
  not relabeled Accepted or independently reviewed by this integration.
- Preserve every identity/context/read audit and final-policy CI step alongside
  process-crash recovery, keeping existing read-only permissions and triggers.
- Keep offline/version/help and local signed Policy CLI commands before HTTP
  client construction; retain both offline help and dependency-cycle tests.
- Scope old single-Worker restart restrictions to memory development mode while
  retaining PostgreSQL coordination and UNKNOWN quarantine.
- Recovery fixtures pin the actual mandatory Policy engine hash so they can
  pass final Grant policy evaluation without weakening its production checks.

## Validation checkpoint

Full ordinary package race/shuffle, whole-repository vet/build, module tidy and
workflow lint passed after feature integration. Affected application/Runner/
Workflow race and integration-tag vet passed. Local packaging tests passed with
one candidate-dependent class skipped; exact committed-source candidate
verification remains covered by the integration PR CI. Dedicated PostgreSQL/
Temporal recovery and the final CI are pending at this checkpoint.

This merge does not constitute a release, deployment, IdP/AWS validation or
completion of every parent Issue. Existing non-production boundaries, missing
live sources, pending ADR review and external gates remain recorded. No Issues
are closed by keyword in the integration PR.
