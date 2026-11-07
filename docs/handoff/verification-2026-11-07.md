# Handoff 2026-11-07 (verification / real-environment / external phase)

State: all executable work is done; remaining items need external input. Nothing here is a pass for
#6/#7/#11/#13/#45/#47 (0 live-cloud runs, 0 external attempts).

## Checked today
- main `152ab71`, CI green, no open PRs. Private vulnerability reporting API reports `enabled: true`.
- No open `good first issue` (#58-#62 all closed) -> #45 needs new beginner issues informed by observation.
- #43 license: upstream `nexus-proto-annotations` tag v0.1.0 has no LICENSE (added 2026-06-29). Recorded in PR #106.

## Needed from the owner (one request)
1. #39: from a non-owner account confirm Security -> Report a vulnerability is visible; send a non-sensitive dummy report; decide primary responder, backup and absence handling.
2. #44: confirm haidouyingtian0@gmail.com is private and reachable; confirm no second reviewer is appointed (not appointed here).
3. Approve scope for outreach to external trial participants (#45/#47), if any.
4. #43: choose resolution for the missing upstream LICENSE (newer tag / upstream confirmation / exclude module).

## Needed for real staging (#6/#7/#11/#13)
Isolated AWS account, one ECS service, IAM/KMS/STS, Datadog (or other) source, CI; region and budget approved.
Optional for #6 only: LocalStack Auth Token (limited proof).
Then: run docs/runbooks/ecs-staging-acceptance.md steps 1-5, store evidence outside the repo, link in Issues.

## After unblock
- #39/#44: update SECURITY.md/MAINTAINERS.md to the confirmed facts, check acceptance criteria, close.
- Staging: preflight + iam-denial first (read-only verifier identity).
- Trial: give participants v0.1.0-alpha.1 checksum/attestation steps; record only observed values in docs/evaluation/results.md; append clean-machine result to docs/guides/offline-verification.md.
