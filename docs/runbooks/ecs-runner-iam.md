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

The adapter uses `DescribeServices` for its typed status and reconciliation
reads, and `UpdateService` for a configured deployment or rollback. It does not
register task definitions, change service settings, or accept provider ARNs
from a Grant. See the [AWS ECS service authorization reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ecs.html)
for resource types and condition keys, and the [UpdateService API reference](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_UpdateService.html)
for provider behavior.
