# CI/CD reference integrations (#11)

Reference material for existing pipelines. **Not validated against a live Control
Plane or cloud**; the gate logic has fake-`themisyctl` tests only. Dedicated staging only.

| Pattern | File | What it does |
|---|---|---|
| GitHub Actions | [themisy-release.yml](../../examples/ci/github-actions/themisy-release.yml) | Verifies the pinned release (checksum + attestation), obtains an OIDC token, plans and starts through the Control Plane, then waits. No cloud credentials in the job |
| Argo CD | [presync-gate.yaml](../../examples/ci/argocd/presync-gate.yaml) | PreSync hook that fails unless the named Run SUCCEEDED for the committed plan hash |

Shared helper: `scripts/themisy_gate.py` (`submit`, `wait`, `verify`).

## Rules the gate enforces

- Success is only Control Plane status `SUCCEEDED`. `FAILED`, `BLOCKED`, `ESCALATED`,
  `CANCELLED` exit 1; `WAITING_APPROVAL`/`PAUSED`/running past the timeout exit 3
  (pending, not success); unknown status or any error exits 2.
- CI success, a finished sync, portal state or `ci_success` fields in the intent are
  never deployment success. Those intent booleans are caller assertions, not authority.
- `verify` also requires the Run's plan hash (and optionally environment) to match
  what was reviewed, so an old successful Run cannot approve a different change.

## What this does not prove

The pipeline identity must not have its own write path. For ECS that is checked by
[staging acceptance](../runbooks/ecs-staging-acceptance.md). Argo CD retains its own
cluster permissions, so the hook is a safeguard against mistakes, not an access
boundary; Themisy has no Argo CD adapter and none is planned before the first
staging gate. Equivalent Plan hashes across REST and MCP are tested
(`TestMCPAndRESTReturnSamePlanHash`); the CLI calls the same REST endpoint.
