# Architecture overview

## Status and scope

This document defines the target OSS v1.0 boundaries. The repository currently contains a non-production prototype with PostgreSQL durable records/read projections, a Temporal Release Workflow, a fixed Go policy engine, and a mock adapter. The in-memory store and synchronous Engine remain compatibility/test implementations. Runner-side enforcement, production identity/policy, and real adapters are still target architecture rather than implemented production boundaries.

Themisy is an Agent Operations Control Layer. It gives human users, CI, CLIs, and AI agents the same deterministic path for planning, authorizing, executing, verifying, stopping, and auditing a change. An AI agent is an interface and delegated actor, not a policy authority, credential holder, or workflow state store.

## System context

```text
Human / CI / CLI / AI Agent
          │ structured intent and authenticated subject
          ▼
┌──────────────── Control Plane trust boundary ────────────────┐
│ Intent API → Context → Planner → Authorization → OPA         │
│                                  │                           │
│ Approval ───────────────→ Temporal Workflow → Audit          │
│                                  │                           │
│                         signed Action Grant                  │
└──────────────────────────────────┼───────────────────────────┘
                                   │ outbound authenticated channel
                                   ▼
┌──────────── Customer Environment Runner boundary ────────────┐
│ Grant verification → local OPA → execution journal           │
│                                    │                         │
│                     Credential Broker → typed Adapter        │
└────────────────────────────────────┼─────────────────────────┘
                                     ▼
                         CI/CD / runtime / observability
```

The Control Plane may plan and authorize an action, but cannot perform a production write by itself. The Runner is the final Policy Enforcement Point and has a separate failure and compromise boundary.

## Component responsibilities

### Interfaces and application layer

REST, CLI, UI, and MCP accept structured intent and invoke the same application use cases. Natural language is resolved before it crosses into execution logic. These interfaces do not call adapters or issue credentials.

Identity verification produces a requester or CI subject. Agent delegation is verified separately and records its issuer, audience, scope, resource selector, environment, risk limit, and expiry. Request-body claims are never authoritative identity, role, scope, CI, or health evidence.

### Context and contracts

Git-hosted Service Contracts are the source of truth for declared service properties. Release Profiles describe reusable execution shapes and required adapter capabilities. Dynamic deployment and health state remains authoritative in the external CI/CD, runtime, or observability system.

Every context item records source revision, observation time, schema version, and integrity hash. A plan pins the relevant Contract, Profile, Policy, and context versions. Safety-relevant changes or expiry invalidate the plan and any bound approval.

### Planner

The Planner validates contracts and typed dependencies, detects cycles and conflicts, checks required capabilities, derives waves and constraints, and creates a canonical plan. Hash input excludes map iteration order, input ordering, generated timestamps, and other nondeterministic values. Planning never performs an external write.

### Authorization and policy

Authorization is the intersection of customer subject permissions and explicit agent delegation. Policy is deterministic and hierarchical:

```text
Platform mandatory policy
  ∩ environment policy
  ∩ team policy
  ∩ service policy
  ∩ current structured facts
```

Lower layers cannot weaken platform mandatory policy. OPA evaluates a typed canonical input in the Control Plane for planning and explanation, then again in the Runner immediately before a write. The Runner uses the pinned, signed bundle. A missing, expired, invalid, or mismatched bundle fails closed.

### Durable workflow

Temporal is the execution authority for active workflow state, timers, signals, retries, and compensation sequencing. Workflow code must be deterministic: wall-clock reads, randomness, network calls, database calls, policy evaluation, signing, and adapter operations belong in Activities or domain/application services with recorded results.

PostgreSQL holds contracts, indexes, durable application records, transactional outbox data, and rebuildable API/UI projections. An append-only audit store holds decision and evidence history. PostgreSQL projections do not independently authorize an external write.

### Action Grant

The Control Plane signs a short-lived Action Grant for one typed step. A grant binds, at minimum:

- tenant, run, step, Runner audience, and target environment;
- requester or CI subject and agent-delegation reference;
- typed action and immutable artifact identity;
- Plan, Contract, Policy Bundle, and required Evidence hashes;
- approval proofs when required;
- idempotency key, unique nonce, issue time, and expiry.

A grant is capability-limited authorization, not a bearer form of a cloud credential. It cannot request shell text, arbitrary HTTP, or a generic provider operation.

### Customer Environment Runner

The Runner is deployed inside the customer's network and connects outbound to the Control Plane using workload identity and an authenticated encrypted channel. Its fixed order is:

1. authenticate the channel and validate the grant signature and audience;
2. verify expiry, nonce uniqueness, hashes, subject proof, approvals, and target;
3. evaluate the pinned policy and mandatory baseline locally;
4. reserve the idempotency key and record intent in the local journal;
5. obtain a short-lived target credential from the Credential Broker;
6. dispatch one allowlisted typed adapter action;
7. reconcile external status and return signed normalized results or evidence.

An API must not make it possible to skip these stages. On Control Plane disconnection, the Runner starts no new production write. It may finish status reconciliation or a pre-authorized compensation only when the grant and local policy explicitly permit it.

### Credential Broker and adapters

Production credentials are created or retrieved in the customer environment after grant and policy validation. They are scoped to the target action and lifetime, never returned to the Control Plane or an agent, and never embedded in grants, audit events, or evidence.

Adapters expose stable, typed high-level operations such as deploy, get status, verify, cancel, and rollback. They accept an allowlist of fields, store external execution IDs, classify retryable, terminal, and unknown results, and reconcile unknown state before retrying. A typed adapter must not grow an escape hatch for arbitrary commands or requests.

## Sources of truth

| Information | Authority |
|---|---|
| service declaration | versioned Service Contract in Git |
| dynamic deployment state | external CI/CD or runtime system |
| active workflow state | Temporal history |
| application indexes and UI/API projection | PostgreSQL, rebuildable from authoritative events |
| policy decision and evidence history | append-only audit store |
| credential material | customer identity/secret system through the Runner's Credential Broker |
| external side effect | external system, reconciled by adapter external ID and idempotency key |

No single database row or agent conversation is sufficient proof that an external action occurred or is safe to begin.

## Trust Boundaries

| Boundary | Inputs treated as untrusted | Required enforcement |
|---|---|---|
| client to Control Plane | intent, agent output, request-body identity facts | authentication, schema validation, delegation verification, limits |
| context sources to Planner | webhook order, stale catalog state, provider responses | signatures where available, revision pinning, freshness, deduplication |
| Control Plane internals | policy input, workflow commands, projections | typed commands, state versioning, OPA, audit-before-grant |
| Control Plane to Runner | Action Grant, result acknowledgement, connection state | mTLS/workload identity, signature, audience, hashes, nonce, expiry |
| Runner to credentials | credential request | local identity, least privilege, short lifetime, no export |
| Runner to external system | typed action and provider response | allowlisted adapter, idempotency, external-ID reconciliation |
| evidence to workflow | metrics, logs, runtime state | provenance, observation window, hash, four-state evaluation |

## Failure semantics

- Unknown authentication, authorization, policy, context freshness, or grant state means no new write.
- `MISSING` and `INCONCLUSIVE` evidence never become `PASS`.
- An audit durability failure prevents grant issuance and a new write.
- An unknown provider result is reconciled by idempotency key or external ID before retry.
- A failed dependency prevents downstream dispatch.
- A failed rollback stops downstream work and escalates.
- A duplicate webhook, command, or grant is deduplicated using a stable external event ID, request key, or nonce.
- A Control Plane compromise alone is insufficient for arbitrary action: the Runner still enforces grant shape, local mandatory policy, target allowlists, and credential scope.

## Deployment forms

The same contracts, protocol, Runner, adapters, and safety tests support:

- Community OSS in a single organization;
- a managed Control Plane with customer-hosted Runners;
- an enterprise self-hosted Control Plane.

Enterprise and managed features may add fleet management, identity integrations, long-term analytics, and operational support. They must not remove core safety enforcement from the OSS Runner or hide required safety invariants behind a commercial tier.

## Runner recovery and independent plan authority

Runner replicas share the customer-owned `themisy_runner` PostgreSQL schema,
separate from Control Plane projections and migrations. A reservation is never
transferred into a second provider write. Recovery claims fence stale writers
and permit read-only observation only. Persisted results are reported back using
the authenticated recovery route, including after a delivery token expires.

The approval/provisioning path signs a versioned Plan Manifest independently of
Action Grant dispatch. Runner configuration selects its own manifest issuer/key
and file distribution path. Verified manifests and monotonic plan revisions are
stored in the shared database; Grant hashes and approval references are compared
against that independent source. Grant-derived ReceivedContexts has been removed.
Plan activation is serialized with database dispatch admission, fencing old
reservations but not retracting already-admitted external calls.

See [the operating runbook](../runbooks/runner-recovery-and-manifests.md) and
[manifest/recovery protocol](../protocols/runner-recovery-and-manifest.md).
