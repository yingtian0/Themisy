# Dedicated ECS staging acceptance (#7 / #11 / #6)

**Not executed.** No dedicated AWS staging environment has been provided, so none of
the steps below has live evidence. This runbook and `scripts/staging_acceptance.py`
prepare the gate; skipped, unavailable or inconclusive results are never a pass.
Do not run this against production or shared accounts. Creating resources costs
money and requires the owner's explicit approval of the account, region and budget.

## Scope

One stateless ECS service in an isolated staging account, one Runner, fixed
cluster/service/role mappings in Runner-local config. Use the role policy in
[ecs-runner-iam.md](ecs-runner-iam.md) and the source procedure in
[source-admission-staging.md](../guides/source-admission-staging.md).

## 1. Read-only preflight

Create a config (no secrets):

```json
{"expected_account_id": "<12 digits>", "region": "ap-northeast-1",
 "cluster": "<stg cluster>", "service": "<stg service>",
 "service_arn": "<service arn>",
 "denied_principals": ["<CI role arn>", "<Agent role arn>"],
 "runner_role_arn": "<runner role arn>",
 "runner_denied_resources": ["<arn of a service outside the allowlist>"]}
```

Using a dedicated read-only verifier identity (never the CI, Agent or Runner one):

```bash
python3 scripts/staging_acceptance.py preflight staging.json --evidence evidence/preflight.json
python3 scripts/staging_acceptance.py iam-denial staging.json --evidence evidence/iam-denial.json
```

Preflight refuses names containing prod/production/live and any account other than
the expected one. `iam-denial` uses `iam simulate-principal-policy`; CI/Agent
principals must be implicit/explicit denied for `ecs:UpdateService`,
`ecs:RegisterTaskDefinition` and `iam:PassRole`, and the Runner role must be denied
`ecs:UpdateService` on a service outside its allowlist. Exit 0 only when every check
is PASS. Simulation is necessary evidence, not sufficient: also complete step 3 and
run IAM Access Analyzer for any other path (permission boundaries, SCPs, resource
policies, other roles that can assume into write access).

## 2. Deploy, verify, roll back

With the Runner running against staging and the signed Policy/Context/identity
material provisioned:

1. Plan, approve and start a deploy of a known-good image digest; confirm the Runner
   executes only the typed operation and the signed runtime/health observation
   reports the new task definition healthy.
2. Start a deploy of a deliberately failing image; confirm failed health blocks
   success and the separately evaluated rollback runs and is re-verified.
3. Record Plan hash, Grant id, Run id, target commit and configuration, expected and
   observed state, audit events and CloudTrail event ids.

## 3. Refusal and bypass attempts (each must fail without a Write)

- Tampered, expired, revoked or wrong-target Grant; revoked delegation; missing or
  stale Context/Evidence → Runner refuses before credentials are obtained.
- From the CI and Agent identities, call `aws ecs update-service` against the
  staging service → AccessDenied (log the denial; do not retry with other roles).
- Runner attempts for a cluster/service/role outside its allowlist → denied.

## 4. Failure and concurrency

Disconnect the Runner, drop the response after a Write, restart the Runner and the
Worker, and run two Runners on one shared journal. Expect no duplicate Write;
UNKNOWN is resolved only by read-only reconciliation
([recovery](runner-recovery-and-manifests.md)). Confirm via CloudTrail that exactly
one `UpdateService` occurred per Grant.

## Migrating an existing pipeline

Phase 1: keep CI deploy, run Themisy Plans in shadow and compare. Phase 2: move one
staging service to Grant-only writes while CI keeps read access. Phase 3: remove the
CI/Agent Write permissions (step 3 must then pass). Roll forward one service at a
time.

## Break-glass

A named human with a separately audited role may update the service directly. Every
use must be recorded (who, why, change, time), reviewed afterward, and followed by a
reconciliation Plan; the break-glass role is excluded from the Agent/CI identities and
is not evidence of the Themisy boundary.

## Evidence and completion

Store the JSON evidence, CloudTrail ids and run records outside the repository
(redact account-specific secrets), and link them from #11 with the commit tested.
Only when every item passes on real resources may #6/#7/#11 acceptance be marked;
absent items stay open.
