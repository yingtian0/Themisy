# Handling expired authorization today

Current Grants are capped by the original Plan's expiry. A 24-hour
approval wait is not 24-hour Write authority. Long observation or a late
approval can leave a later deploy or rollback unable to obtain a Grant.
Do not extend Plan timestamps, reuse approval with a new interval, disable hash
checks, change idempotency keys, or clear reservations to force an operation.

1. Stop new dispatch in the affected scope through supported pause/freeze
   controls. Inventory known Grants and whether a Write started; a pause alone
   does not recall an outstanding signed Grant.
2. For in-flight/UNKNOWN operations, use the
   [manifest/recovery runbook](runner-recovery-and-manifests.md) and read-only
   provider observations. Preserve signed results, journal and audit evidence
   even when the Grant/Plan has expired. Do not issue another Write to discover
   the outcome.
3. If a source outcome or approved rollback target is unknown, keep it stopped
   for authenticated operator handling. An expired rollback window is not an
   emergency bypass permission.
4. Once source outcomes are known, collect fresh trusted Context, create a new
   Plan for remaining intended changes and obtain new approval/manifest. Do not
   mutate the immutable production Plan or directly edit projection rows. Do
   not include completed or unresolved operations as accidental new writes.
5. Record the old/new Run and Plan relation, relevant deadlines, observations,
   decision and remaining limitations. Current APIs do not implement automatic
   successor Run creation. Bounded compensation is described below.

## Provisioning bounded ECS compensation

Review a YAML file with `service`, `environment`, `region`, `cluster_arn`,
`service_arn`, `rollback_task_definition`, and `rollback_container_digests`
(a container-name to immutable image-digest map). It must match the independently
managed Runner ECS target configuration. Run:

```sh
go run ./cmd/rollbacktarget -config predecessor.yaml
```

This computes a public hash without provider calls. Put the hash in the Contract
environment's `rollbackTargetHash`, review `rollback.deadline`, then create a
fresh Plan and approval. After the deploy result is durably succeeded, provision
the independent rollback manifest with `cmd/planmanifest -source-grant` naming
that exact source (plus the normal Run/step/external execution/signing options).
Do not advertise automatic rollback until the manifest is available. An absent
manifest or predecessor stops rollback before new Write.

The permission expires at the earlier of source Grant issue time plus Contract
window and all original authority deadlines. It is consumed once per source at
Runner reservation, including a later failure before Write. Do not reset this
consumption or issue another key to retry. Expired authority requires fresh
operator-reviewed planning; automatic post-expiry rollback is not supported.

Apply Control Plane migration 14 and Runner migration 008 before v2 issuance.
Drain legacy issuers and preserve UNKNOWN records for read-only recovery. New
source-enabled Runners reject old Write Grants without an authorization epoch;
old manifests cannot be relabeled as v2. See [ADR-0011](../adr/0011-authorization-and-compensation-lifetimes.md)
for signature compatibility, expiry reasons and rollout limits.
