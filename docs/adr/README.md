# Architecture Decision Records

Architecture Decision Records (ADRs) capture decisions that materially affect Trust Boundaries, public contracts, durable state, identity, policy, credentials, cryptography, or operations.

## Index

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-themisy-agent-operations-control-layer.md) | Accepted | Build an Agent Operations Control Layer, not an AI-specific deploy tool |
| [0002](0002-temporal-workflow-engine.md) | Accepted | Use Temporal as the durable workflow engine |
| [0003](0003-opa-policy-engine.md) | Accepted | Use OPA with Control Plane evaluation and final Runner enforcement |
| [0004](0004-customer-environment-runner.md) | Accepted | Keep credentials and final write enforcement in a customer-hosted Runner |
| [0005](0005-postgresql-and-audit-state.md) | Accepted | Use PostgreSQL for durable application state and projections, with append-only audit history |
| [0006](0006-action-grant-and-identity-signatures.md) | Accepted | Canonical Ed25519 Action Grant and Policy Bundle signatures with verified OIDC JWS subjects |
| [0007](0007-first-deployment-adapter.md) | Accepted | Use a locally allow-listed GitHub Actions workflow as the first typed deployment adapter |
| [0008](0008-runner-grant-transport-and-ecs.md) | Accepted | Dispatch signed grants over an outbound Runner channel and resolve ECS targets locally |

## Process

1. Copy the structure of an existing ADR and assign the next four-digit number.
2. State the problem, constraints, options, decision, consequences, and safety implications.
3. Open the ADR before or with implementation; do not use implementation as the only record of a boundary change.
4. Record meaningful objections and alternatives. An accepted ADR is immutable except for corrections and links.
5. Supersede a decision with a new ADR and update this index. Do not rewrite history to make an earlier decision appear different.

Statuses are `Proposed`, `Accepted`, `Rejected`, `Deprecated`, or `Superseded by ADR-NNNN`. Governance and approval requirements are defined in [GOVERNANCE.md](../../GOVERNANCE.md).
