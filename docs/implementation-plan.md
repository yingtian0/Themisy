# OSS v1.0 implementation plan

This document replaces the earlier prototype-first staging plan. Temporal and OPA are architectural commitments for OSS v1.0; they are not optional migrations deferred until after a PostgreSQL worker design.

Current priorities and release gates are in the [implementation strategy](implementation-strategy.md).
Use the [common workflow](agent-operations/README.md); GitHub Issues own task state
and acceptance evidence. Dated handoffs are historical snapshots.

## Delivery model

Work is divided into seven bounded Task Packets. A change set implements one packet only. Before changing code, contributors record the current worktree and baseline test result, read the packet's required material, and preserve existing user changes. A packet is complete only when its required verification succeeds and remaining risks are recorded.

| Order | Packet | Scope |
|---:|---|---|
| 00 | Foundation and architecture baseline | Trust Boundaries, ADRs, threat model, governance, CI |
| 01 | Contract and planner | Service Contract v1alpha1, Release Profile v1alpha1, typed dependency graph, canonical Plan v2 |
| 02 | Durable core | PostgreSQL migrations and repositories, Temporal workflows, projections, recovery |
| 03 | Runner, identity, and policy | Customer Environment Runner, signed Action Grants, replay protection, OIDC delegation, OPA |
| 04 | Adapters and verification | one production-quality deploy path, Datadog evidence, verification, rollback |
| 05 | Interfaces and scale | REST API, CLI, MCP facade, status UI, concurrency budgets, circuit breaker, 200-service scenarios |
| 06 | Security and release | packaging, Helm and Compose, observability, hardening, SBOM, signed OSS v1.0 release |

Do not create empty target packages or placeholder safety checks ahead of the packet that owns them. Safety-critical paths must fail closed; a future implementation marker must never permit a write.

## Milestones

| Milestone | Target | Exit outcome |
|---|---:|---|
| M0 Architecture Baseline | 2026-08-30 | reviewed boundaries, governance, and CI |
| M1 Declarative Planning | 2026-09-27 | contracts, profiles, typed graph, reproducible Plan v2 |
| M2 Durable Core | 2026-10-25 | PostgreSQL state, Temporal replay and recovery |
| M3 Local Safety Boundary | 2026-11-22 | Runner, grants, delegation, and local OPA enforcement |
| M4 Real Adapter Alpha | 2026-12-20 | staging deploy, evidence, verification, and rollback |
| M5 End-to-End Beta | 2027-01-17 | common domain commands through CLI, HTTP, MCP, and UI |
| M6 Scale Candidate | 2027-01-31 | concurrency safety and 200-service scenarios |
| M7 Release Candidate | 2027-02-14 | security review, packaging, operations documentation |
| M8 Public OSS Release | 2027-03-01 | signed artifacts and OSS v1.0.0 |

Targets may move when a safety or security gate is not met. Scope is reduced before a safety invariant is weakened.

## Architecture sequence

```text
Architecture and threat model
    ├── Service Contract → typed planner ───────────┐
    ├── PostgreSQL → Temporal workflow ─────────────┤
    └── identity and signing → customer Runner ─────┤
                                                     ↓
                             deploy adapter → verification
                                                     ↓
                                  API / CLI / MCP / status UI
                                                     ↓
                              scale tests → security → OSS v1.0
```

The [architecture overview](architecture/overview.md) defines component boundaries. Accepted choices and their consequences are indexed in [the ADR directory](adr/README.md). The [threat model](security/threat-model.md) defines required failure behavior.

## Permanent safety invariants

1. No execution is created without a valid policy `ALLOW` or a valid approval bound to the plan.
2. No external write occurs without the Runner's final local policy `ALLOW`.
3. A downstream step does not start before every required dependency succeeds.
4. An external write occurs at most once for the same idempotency key.
5. Production writes record the requester or CI subject separately from agent delegation.
6. Production writes require pinned Plan, Contract, and Policy hashes.
7. Expired, modified, or replayed Action Grants are rejected.
8. Missing or inconclusive evidence is never converted to success.
9. A rollback failure stops downstream work and escalates.
10. Audit persistence failure prevents a new Action Grant or external write.
11. Loss of the Control Plane or fresh required context prevents new production writes.
12. Platform mandatory policy cannot be weakened by environment, team, or service policy.

## Standard verification

Select local checks for the changed packages and failure boundaries. These are
the full-suite targets used by CI; running all of them locally is not required
for every change:

```bash
make fmt-check
make test
make test-race
make lint
```

Integration and scenario tests that require PostgreSQL, Temporal, OPA, or external mock services remain separate targets once those components are introduced. A skipped or unavailable check is reported as unverified, not as passing.

## Definition of done

A packet is done when:

- implementation and user-facing documentation agree with the accepted ADRs;
- failure-path tests cover new safety behavior;
- race, formatting, test, and vet checks pass;
- API and persistence compatibility have been assessed;
- secrets, credentials, PII, and audit data handling have been reviewed;
- changes and unresolved issues are reported without beginning the next packet.
