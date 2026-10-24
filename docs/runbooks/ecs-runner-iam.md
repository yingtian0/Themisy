# ECS Runner role policy example

The Runner's base workload role should only assume the purpose-specific deploy
and rollback roles configured for its local target mapping. It must not receive
`ecs:UpdateService` directly. Give the deploy and rollback roles their own
policies and trust them only from that Runner workload identity.

The following is a starting point for one ECS service. Replace the account,
region, cluster, service, and task-role ARNs with the values in the Runner's
trusted local configuration. Keep the deploy and rollback task-definition
mappings separate even if a customer chooses to grant identical service-level
permissions to both roles.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ObserveOneService",
      "Effect": "Allow",
      "Action": "ecs:DescribeServices",
      "Resource": "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment",
      "Condition": {
        "ArnEquals": {
          "ecs:cluster": "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod"
        }
      }
    },
    {
      "Sid": "ReadTaskDefinitionContent",
      "Effect": "Allow",
      "Action": "ecs:DescribeTaskDefinition",
      "Resource": "*"
    },
    {
      "Sid": "ListClusterTasks",
      "Effect": "Allow",
      "Action": "ecs:ListTasks",
      "Resource": "*",
      "Condition": {
        "ArnEquals": {
          "ecs:cluster": "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod"
        }
      }
    },
    {
      "Sid": "DescribeClusterTasks",
      "Effect": "Allow",
      "Action": "ecs:DescribeTasks",
      "Resource": "arn:aws:ecs:ap-northeast-1:111122223333:task/prod/*",
      "Condition": {
        "ArnEquals": {
          "ecs:cluster": "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod"
        }
      }
    },
    {
      "Sid": "UpdateOneService",
      "Effect": "Allow",
      "Action": "ecs:UpdateService",
      "Resource": "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment",
      "Condition": {
        "ArnEquals": {
          "ecs:cluster": "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod"
        }
      }
    }
  ]
}
```

If the selected task definition requires `iam:PassRole`, add a separate
statement naming only its task and execution roles. Restrict
`iam:PassedToService` to the ECS service principal(s) required by that task
definition. Do not use `Resource: "*"` for `iam:PassRole`. Whether a policy
works for a particular launch type and task definition must be confirmed in the
customer staging account; this example is not evidence of a live authorization
test.

The adapter uses `DescribeServices` for typed status and `DescribeServices`,
`ListTasks`, and `DescribeTasks` for deployment/rollback success and read-only
reconciliation. This example uses `Resource: "*"` with a cluster condition for
service-wide `ListTasks` without a container-instance filter; it permits task
discovery within that cluster. The AWS authorization reference also lists the
container-instance resource type for ListTasks. This example has not been
validated against live IAM. The adapter uses `UpdateService` for a configured
deployment or rollback. It does not
register task definitions, change service settings, or accept provider ARNs
from a Grant. See the [AWS ECS service authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ecs.html)
for resource types and condition keys, and the [UpdateService API reference](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_UpdateService.html)
for provider behavior.

## Runtime success gate (simulated verification only)

Configure `expected_container_digests` for every deployment task definition and
`rollback_container_digests` for the rollback definition in the trusted Runner
configuration. These maps are container name to immutable SHA-256 image digest;
they cannot be supplied by a Grant. Include every sidecar explicitly, with its
own digest. Missing maps, missing containers, extra containers, duplicate names,
or mismatched/missing digests cannot succeed. An artifact digest can identify a
release separately from the image digests of its individual containers.

Deploy, Rollback, and their read-only Reconcile require an ACTIVE service with
the intended task definition, positive desired count, running equal to desired,
and zero pending tasks. A single PRIMARY deployment must have the intended
definition, COMPLETED rollout, matching desired/running counts and no pending
tasks. The Runner then lists RUNNING-desired tasks by the trusted service name
and describes every listed task. Both task desired/last status and every
container last status must be RUNNING, task and container health must be
HEALTHY, and task cluster/service/definition and container task identity must
match. Tasks or containers without ECS health checks can report UNKNOWN; that
is insufficient evidence and deliberately cannot succeed.

Each observation uses at most 10 ListTasks pages, at most 1000 tasks, and
DescribeTasks batches of 100. A larger service, repeated pagination token,
partial/failure response, duplicate or foreign response, API error, or an
incomplete rollout cannot become success. After a write is sent, an ambiguous
response or inconclusive runtime observation returns UNKNOWN. UpdateService
acceptance and an already selected desired definition are never success
shortcuts. Recovery uses read-only Reconcile; it must never resend UpdateService.
The gate is a point-in-time provider observation, not a guarantee of future health.

These checks are covered with simulated ECS responses only, not live AWS/IAM
validation. This is one Issue #9 unit: workflow waiting currently stops on
UNKNOWN and the later runtime Verification/Evidence integration remains
unconnected. Passing this adapter gate does not claim all of Issue #9 complete.


## Task definition preflight

Deploy and rollback now require `DescribeTaskDefinition` before any
`UpdateService`. The configured exact ARN must resolve to an ACTIVE definition
with exactly the configured container names and immutable `repository@sha256:...`
images. A family name, mutable image tag, missing/extra container, wrong revision
or digest cannot pass. Provider read failure makes no write; the existing runtime
health check is still required after dispatch. No task-definition registration
or service-setting edits are added.

Existing role policies must add the read action before adopting this adapter.
The AWS authorization table lists no resource type for this read action, so the
example grants only `ecs:DescribeTaskDefinition` on `*`; this is broader read
visibility, not a write permission. Confirm its acceptability for the customer
account and validate IAM in staging. The application still requests only its
locally configured revision. Custom implementations of the internal `ecs.API`
interface must implement the narrow DescribeTaskDefinition method. Grant and
public Adapter SDK request formats are unchanged.

Sources: [AWS ECS authorization table](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ecs.html),
[DescribeTaskDefinition API](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_DescribeTaskDefinition.html).
