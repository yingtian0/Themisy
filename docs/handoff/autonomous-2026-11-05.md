# Handoff 2026-11-05

Workspaces: primary `/Users/yingtian0/workspace/themisy` (branch `issue-5-runner-transport`, user's
uncommitted docs untouched); work worktree `/Users/yingtian0/workspace/themisy-wt-main`.

Merged this session: #98 (readiness/README), #99 (candidate reproduction), #100
(MAINTAINERS/CoC contact, owner-supplied), #101 (manual provenance workflow), #102
(staging preflight/IAM-denial helper and runbook). Private vulnerability reporting was
enabled by the owner's instruction (2026-11-05).

Open and why:
- #39: external-reporter reachability and dummy report test; primary/backup responders.
- #44: owner to confirm the CoC mailbox is private and reachable; second reviewer not appointed.
- #43: publication decision (tag/Release), execution image, license review, independent clean-machine run.
- #45/#47: no external participants yet (results ledger: 0 attempts).
- #6/#7/#11/#13: need real staging (AWS, KMS/STS, Datadog/CI sources) or a LocalStack Auth Token; run `docs/runbooks/ecs-staging-acceptance.md`.

Next with no external input: GitHub Actions/Argo CD reference integration (#11), CLI/REST/MCP hash equivalence is covered for MCP/REST (CLI is a thin REST client), ADR proposals stay Proposed.
