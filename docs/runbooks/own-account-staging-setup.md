# Own-account ECS staging setup (single AWS account)

Prepares the environment that [ecs-staging-acceptance.md](ecs-staging-acceptance.md) runs against
(#6 / #7 / #11 / #13). **Not executed.** Creating resources costs money and needs the owner's
explicit approval of account, region and budget. Nothing here is evidence.

Account `576770687339`, region `ap-northeast-1`. The verification user is IAM user `themisy-operator`.

## Limits of a single-account setup (state these in the evidence)

- Isolation is by IAM only. An administrator of this account can bypass every boundary, so the
  result proves the *role/policy design*, not account-level isolation. Use an account that hosts
  nothing else; never the root user or root access keys (delete any that exist).
- No Datadog: runtime/health come from ECS itself. The #13 external-source course (real CI run,
  dependency metric) is **not** covered and stays open.
- Skipped / inconclusive steps are never PASS.

## 0. Before creating anything

1. An administrator identity (not `themisy-operator`, not root) creates the resources below.
2. AWS Budgets: monthly cost budget ~USD 7, alert at 80%, email.
3. Approve the cost table (section 6).

## 1. Identities

| Identity | Kind | Purpose | Allowed |
|---|---|---|---|
| `themisy-operator` | existing IAM user | read-only verifier; runs `staging_acceptance.py`; never holds Write | policy A + probe-assume (B) |
| `themisy-runner-base` | IAM user | Runner workload base identity; access key only in the Runner's protected file | `sts:AssumeRole` on deploy/rollback roles only |
| `themisy-deploy-role`, `themisy-rollback-role` | roles | typed ECS operations; trusted only by `themisy-runner-base` | policy from [ecs-runner-iam.md](ecs-runner-iam.md) for one service |
| `themisy-ci`, `themisy-agent` | roles | denial probes; trusted by `themisy-operator`; **no** ecs write / RegisterTaskDefinition / PassRole | `ecs:Describe*` only |

Never give the Control Plane, Agent or CI the Runner key. Never print or paste keys.

### Policy A: `themisy-verifier-readonly` (attach to `themisy-operator`)

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "WhoAmI", "Effect": "Allow", "Action": "sts:GetCallerIdentity", "Resource": "*"},
    {"Sid": "ObserveOneService", "Effect": "Allow",
     "Action": ["ecs:DescribeServices", "ecs:ListTasks", "ecs:DescribeTasks"], "Resource": "*",
     "Condition": {"ArnEquals": {"ecs:cluster": "arn:aws:ecs:ap-northeast-1:576770687339:cluster/themisy-stg"}}},
    {"Sid": "ReadTaskDefinitions", "Effect": "Allow",
     "Action": ["ecs:DescribeTaskDefinition", "ecs:ListTaskDefinitions"], "Resource": "*"},
    {"Sid": "SimulateDenialOnly", "Effect": "Allow",
     "Action": ["iam:SimulatePrincipalPolicy", "iam:GetRole", "iam:ListAttachedRolePolicies",
                "iam:ListRolePolicies", "iam:GetRolePolicy"],
     "Resource": [
       "arn:aws:iam::576770687339:role/themisy-ci",
       "arn:aws:iam::576770687339:role/themisy-agent",
       "arn:aws:iam::576770687339:role/themisy-deploy-role",
       "arn:aws:iam::576770687339:role/themisy-rollback-role"]},
    {"Sid": "CloudTrailRead", "Effect": "Allow", "Action": "cloudtrail:LookupEvents", "Resource": "*"},
    {"Sid": "AccessAnalyzerRead", "Effect": "Allow",
     "Action": ["access-analyzer:ListAnalyzers", "access-analyzer:ListFindings", "access-analyzer:GetFinding"],
     "Resource": "*"}
  ]
}
```

### Policy B: probe-assume (also on `themisy-operator`)

Lets the verifier act *as* CI/Agent to prove `ecs:UpdateService` is denied. Those roles hold no write.

```json
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": "sts:AssumeRole",
  "Resource": ["arn:aws:iam::576770687339:role/themisy-ci", "arn:aws:iam::576770687339:role/themisy-agent"]}]}
```

Trust policy for `themisy-ci` / `themisy-agent`:

```json
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow",
  "Principal": {"AWS": "arn:aws:iam::576770687339:user/themisy-operator"}, "Action": "sts:AssumeRole"}]}
```

Trust policy for `themisy-deploy-role` / `themisy-rollback-role` (Runner only):

```json
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow",
  "Principal": {"AWS": "arn:aws:iam::576770687339:user/themisy-runner-base"}, "Action": "sts:AssumeRole"}]}
```

`themisy-runner-base` policy: `sts:AssumeRole` on exactly those two role ARNs, nothing else.
The deploy/rollback permission policy is the one in ecs-runner-iam.md with account
`576770687339`, cluster `themisy-stg`, service `themisy-stg-svc`; add a PassRole statement for
`themisy-task-exec` only, with `iam:PassedToService` = `ecs-tasks.amazonaws.com`.

## 2. ECS resources (administrator)

- Cluster `themisy-stg`; log group `/ecs/themisy-stg` (retention 7 days).
- Role `themisy-task-exec` with AWS managed `AmazonECSTaskExecutionRolePolicy`.
- Network: default VPC public subnets, a security group with **no inbound rules**, public IP on
  (avoids NAT gateway/ALB cost). Do not create a NAT gateway or load balancer.
- Image: a small public image **pinned by digest** (`repo@sha256:...`; read the digest with
  `docker buildx imagetools inspect <image>`). Every container needs an ECS `healthCheck`; a
  container without one reports UNKNOWN and can never succeed.
- Task definition `themisy-stg:1` (known good): Fargate, 256 CPU / 512 MB, healthcheck that passes.
- Task definition `themisy-stg:2` (deliberately failing): same image digest, healthcheck that
  always fails — used to drive the failed-health -> rollback course.
- Service `themisy-stg-svc`: desired count 1, task definition 1, Fargate.
- The "outside the allowlist" service (`other-svc`) is only an ARN string for the simulation;
  do not create it.

Record the image digests: they go into the Runner-local `expected_container_digests` /
`rollback_container_digests` (never from a Grant).

## 3. Keys

Signing keys (Grant, Policy, Directory, build, manifest) follow the guides
([policy](../guides/policy-distribution.md), [identity](../guides/production-identity.md),
[source](../guides/source-admission.md)) and must be separate keys with separate signers; the
Control Plane and Runner must not hold signing permission. Each KMS key costs about USD 1/month.
Create only the keys the executed course needs and decide the count before approving cost.

## 4. Run the acceptance

`staging.json` (outside the repository, e.g. `~/themisy-evidence/staging.json`):

```json
{"expected_account_id": "576770687339", "region": "ap-northeast-1",
 "cluster": "themisy-stg", "service": "themisy-stg-svc",
 "service_arn": "arn:aws:ecs:ap-northeast-1:576770687339:service/themisy-stg/themisy-stg-svc",
 "denied_principals": ["arn:aws:iam::576770687339:role/themisy-ci",
                       "arn:aws:iam::576770687339:role/themisy-agent"],
 "runner_role_arn": "arn:aws:iam::576770687339:role/themisy-deploy-role",
 "runner_denied_resources": ["arn:aws:ecs:ap-northeast-1:576770687339:service/themisy-stg/other-svc"]}
```

Run as `themisy-operator`, evidence outside the repository:

```bash
python3 scripts/staging_acceptance.py preflight ~/themisy-evidence/staging.json --evidence ~/themisy-evidence/preflight.json
python3 scripts/staging_acceptance.py iam-denial ~/themisy-evidence/staging.json --evidence ~/themisy-evidence/iam-denial.json
```

Then follow ecs-staging-acceptance.md sections 2-4. Direct-call probe (expected AccessDenied,
do not retry with another role): assume `themisy-ci` / `themisy-agent` through `themisy-operator`
and run `aws ecs update-service` against the staging service. Count `UpdateService` events per
Grant with `cloudtrail lookup-events` (management events, last 90 days only: save event ids early).

## 5. Mapping to the issues

| Issue | Covered here | Not covered |
|---|---|---|
| #7 typed ECS adapter | deploy/health/rollback on a real service | - |
| #6 STS broker | real AssumeRole into deploy/rollback roles | KMS-bound parts need section 3 |
| #11 | CI/Agent denial, Runner allowlist, single Write per Grant | existing CI/CD pipeline shadow run |
| #13 | - | real CI run/attempt, dependency metric source |

## 6. Cost (approximate, verify on the AWS pricing pages)

| Item | Estimate |
|---|---|
| Fargate 0.25 vCPU / 0.5 GB, one task | about JPY 2-3 per hour while running |
| Public IPv4 | about JPY 0.7 per hour while running |
| KMS keys | about USD 1 per key per month (prorated; deletion has a 7-day wait) |
| STS, IAM, CloudTrail event history | free |
| Logs, small storage | a few JPY |
| NAT gateway / load balancer | not used (would be far larger) |

Roughly JPY 200-1,500 for a week depending on key count.

## 7. Teardown

Set desired count 0 and delete the service, task definitions (deregister), cluster, log group;
schedule KMS key deletion; delete `themisy-runner-base` access keys and the deploy/rollback/ci/agent
roles. Keep the evidence files outside the repository.
