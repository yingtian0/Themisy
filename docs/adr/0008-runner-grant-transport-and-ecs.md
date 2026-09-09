# ADR-0008: Dispatch signed grants over an outbound Runner channel and resolve ECS targets locally

- Status: Accepted
- Date: 2026-09-04

## Context

The Control Plane could plan, authorize, and persist releases, while the Runner could validate an Action Grant and call a typed adapter. Those halves were not connected: workflow activities still used an in-process mock executor, no durable dispatch lease or ACK existed, and the Runner binary did not assemble its security dependencies. The first adapter was GitHub Actions, but direct ECS deployment and AWS workload-identity exchange were also required.

The Control Plane must not receive customer AWS credentials. A compromised agent or Control Plane request must not be able to put a cluster ARN, service ARN, task definition ARN, IAM role ARN, or arbitrary AWS API payload into a Grant. An ambiguous response to a write must not cause an automatic second write.

## Decision

An authorized workflow step is converted to a canonical signed Action Grant by the application layer. Grant creation, append-only audit, and a `grant.dispatch.requested` Outbox event are committed atomically. The Grant record has a renewable delivery lease, authenticated Runner ID, ACK, and terminal Result. A Runner initiates an outbound HTTPS long-poll, ACKs before execution, invokes the existing `Runner.Execute` validation pipeline, and posts its durable Result. A lost connection causes lease-based redelivery; the Runner journal's nonce and idempotency reservation prevents a second provider write.

Control Plane signing uses either an asymmetric Ed25519 AWS KMS key or an explicitly provisioned mode-0600 development key. The AWS KMS implementation uses the official AWS SDK for Go v2, `ED25519_SHA_512`, and `RAW` messages. Runner authentication uses per-Runner bearer secrets loaded from protected files. Production Runner configuration requires HTTPS, durable journal state, grant/OIDC/policy trust keys, and a delegation snapshot.

The ECS adapter accepts only a logical `(service, environment)` and an artifact digest from the Grant. Runner-local configuration maps that tuple and digest to a fixed Region, cluster ARN, service ARN, allowed predecessor task definitions, desired task definition, rollback task definition, and IAM role. The adapter calls `DescribeServices`, checks the current task definition against the configured predecessor set, and then calls `UpdateService` with SDK retry attempts fixed to one. Any error or incomplete response after `UpdateService` is classified as `UNKNOWN_EXTERNAL_STATE`; only `Reconcile` may determine whether the desired task definition became active.

The Runner obtains temporary AWS credentials from its workload identity with STS `AssumeRole`. The role is selected from the same local allowlist. The Grant ID is placed in both the role session name and the `ThemisyGrantID` session tag for CloudTrail correlation. Temporary credentials stay inside the Runner and the opaque credential type.

## Alternatives considered

### Control Plane pushes directly into the customer network

Rejected because it requires inbound customer firewall access and weakens the customer-hosted trust boundary. Outbound long-poll works through ordinary egress controls and supports simple reconnect semantics.

### Put ECS ARNs or an `UpdateService` payload in the Grant

Rejected because it turns a typed authorization into a generic cloud write proxy. Logical names and digests are useful intent; provider identifiers remain operator-controlled Runner configuration.

### Retry `UpdateService` after a timeout or 5xx response

Rejected because the service may have accepted the first request. The action becomes UNKNOWN and reconciliation uses the read-only `DescribeServices` path.

### Use long-lived AWS access keys

Rejected. The official AWS SDK v2 default credential chain supplies the Runner workload identity, and STS issues short-lived credentials for a locally selected role.

## Consequences

- Each Runner requires a protected Control Plane token, trust material, a delegation snapshot, and durable PostgreSQL journal state in production.
- ECS releases require an operator-maintained mapping from artifact digest to task definition and allowed predecessor definitions.
- IAM trust policies must allow `sts:AssumeRole` and session tagging for the configured role.
- KMS signing payloads are limited to the KMS raw-message limit; oversized Grants fail closed.
- The transport is pull-based and uses bounded database polling. A notification mechanism can reduce idle query volume later without changing the protocol.
- Live AWS KMS, STS, and ECS calls require deployment-owned AWS resources and are not exercised by repository tests; SDK boundaries and safety behavior are covered with deterministic fakes.

## Safety implications

No Grant is created without a durable authorized Step, pinned Plan/Contract/Profile/Policy/Evidence hashes, valid approval when required, satisfied dependencies, user or CI identity proof, and delegation reference. Grant creation fails if its audit/Outbox transaction fails. The Runner repeats signature, issuer, audience, tenant, OIDC, delegation, capability, policy, hash, approval, nonce, and idempotency checks before any adapter call. Provider identifiers and AWS credentials never cross the Control Plane/Runner boundary.
