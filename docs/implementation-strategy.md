# Themisy implementation strategy

Last reviewed: 2026-10-15 (Asia/Tokyo)

## Purpose of this document

This is the canonical document for implementation priority, scope control, and
release gates. It intentionally does not duplicate every current file, branch,
commit, or open issue.

- Architecture and trust boundaries belong in `docs/architecture/overview.md`.
- Irreversible design decisions and alternatives belong in `docs/adr/`.
- Safety threats and required mitigations belong in
  `docs/security/threat-model.md`.
- Dates and broad release milestones belong in `docs/implementation-plan.md`.
- Current task state, commit, evidence, dependencies and next action belong in
  GitHub Issues. `docs/handoff/` contains historical snapshots only.
- GitHub Issues are the source of truth for acceptance criteria and remaining
  task-level work.

When these documents disagree, accepted ADRs and the threat model define the
safety boundary; current code and tests define what is actually implemented.
A handoff document is a snapshot, not an architecture decision.

## Product focus

Themisy is not another workflow orchestrator. Its first sellable OSS outcome is
a last-mile Production Write boundary with one reference action:

> An agent or CI system can request `ecs.deploy`, but cannot obtain production
> credentials or choose an arbitrary AWS operation. After authorization, a
> customer-hosted Runner executes one allow-listed change, reconciles the real
> ECS state, and returns verifiable evidence.

Temporal, Port, Argo CD, GitHub Actions, and existing CI/CD may initiate or
coordinate work. They do not replace the Runner's final authorization,
credential, journal, typed-adapter, and runtime-verification boundaries.

The guarantee applies only when ordinary agent and CI identities have no
parallel Production Write permission. The Runner role is the normal write path;
a separately controlled human break-glass role is outside the agent path. A
compromised Runner remains inside the trusted computing base and is limited by
workload isolation, local allowlists, short credential lifetime, and provider
IAM.

## Current baseline

The repository has implemented the first execution spine:

- durable issuance and leasing of signed Action Grants;
- outbound Runner long-poll, acknowledgement, execution, and result submission;
- ordered Runner verification with PostgreSQL-backed nonce/idempotency
  reservation before write;
- an STS broker that selects roles from Runner-local configuration;
- a typed ECS adapter that resolves provider identifiers locally and does not
  blindly retry an ambiguous `UpdateService` response;
- PostgreSQL/Temporal application state, typed GitHub Actions deployment, and
  Datadog verification foundations.

This is still a non-production prototype. Repository tests use deterministic
fakes for AWS. Live KMS, STS, ECS deploy/reconcile/rollback, production identity
integration, result signing, full runtime health evidence, and operational
recovery have not passed a dedicated staging release gate.

## Priority order

Current order: **#4 / #5 / #14 → #6 / #7 / #8 → #9 → #11**.
Implement #10 / #12 / #13 only as needed by those primary features. Finish the
current #8 acceptance inventory before selecting the next dependency. The P0
sections below describe release gates, not a competing scheduling order.

### Issue ownership (avoid duplicate implementation)

| Issue | Sole implementation owner |
|---|---|
| #4 | Action Grant format, Control Plane signing, and Grant-key lifecycle |
| #5 | Runner transport authentication, heartbeat, capacity, freeze, and drain |
| #6 | Runner-local AWS credential acquisition and STS session attribution |
| #7 | Typed ECS write/read adapter and trusted target mapping |
| #8 | Shared PostgreSQL Runner journal, write ownership, and three-way reconciliation |
| #9 | Runtime success criteria and signed Result/Evidence |
| #10 | Policy Bundle distribution, shadow evaluation, rollout, and rollback |
| #11 | Cross-component and live-staging verification only; it does not reimplement #4-#10 |
| #12 | Human, CI, Agent delegation, role, tenant, OIDC, and JWKS trust chain |
| #13 | Trusted pre-dispatch CI, dependency, artifact, Contract, and Profile context |
| #14 | Independently approved, shared signed Plan/Context manifests and revision fencing |

An end-to-end assertion may appear in #11 even when its mechanism belongs to a
component issue. In that case #11 consumes the component and owns only the
cross-boundary or live-environment test, avoiding a second implementation.

### P0. Complete execution truth and crash recovery

Primary issue: #8.

Implement a reconciliation protocol that compares:

1. the Runner journal;
2. the Control Plane grant-dispatch record;
3. the external ECS runtime state.

The Runner journal is a shared, customer-managed PostgreSQL database for every
Runner replica in the same tenant/Runner-group trust boundary. It is not a
process-local file and not the Control Plane UI/API projection. Production must
not fall back to the in-memory journal. See ADR-0009.

The protocol must handle a process crash or lost response after AWS accepted a
write. It must never use another write to discover the outcome. Conflicts are
recorded as audit events and require an explicit operator decision when the
read-only provider state cannot resolve them.

Likely code areas:

- `internal/runner/reconcile.go`
- `internal/transport/runner.go`
- `internal/store/store.go`
- `internal/store/postgres/grants.go`
- `migrations/` or a new Runner-only migration package
- `adapters/ecs/reconcile.go`
- `test/integration/`

Exit gate: after Runner and Control Plane restarts at every write boundary, the
same grant causes at most one `UpdateService` call and converges to a durable,
audited terminal or explicitly unresolved state. The same test must run with at
least two Runner processes sharing PostgreSQL and prove that only one replica
can reserve and dispatch the write.

### P0. Make the trust chain operable

Primary issues: #4, #5, and #12.

- Replace static Runner bearer authentication with mTLS or workload identity.
- Add Runner heartbeat, capacity, freeze, and drain semantics.
- Implement KMS key rotation, revocation, key-ID lifecycle, and tenant-specific
  trust discovery.
- Implement OIDC/JWKS refresh and revocation and obtain approver roles from a
  trusted organization source.
- Sign canonical Runner Results and Evidence and verify them before the Control
  Plane accepts a terminal result.
- Audit Runner authentication failures, dispatch, acknowledgement, rejection,
  freeze, drain, and result verification.

Exit gate: an expired, revoked, incorrectly addressed, tampered, or unsigned
identity, delegation, Grant, Result, or Evidence object causes zero adapter
writes and cannot advance workflow state.

### P0. Prove the ECS boundary in dedicated AWS staging

Primary issues: #6, #7, #9, and #11.

- Add Grant ID, run ID, service, and environment to allowable STS session
  correlation data without exposing secrets.
- Verify task-definition contents and immutable image digests.
- Treat ECS API acceptance as dispatch, not deployment success.
- Verify rollout state, desired/running counts, task image digests, and health.
- Exercise deploy, ambiguous response reconciliation, failure, and rollback in
  a dedicated AWS account.
- Store sanitized CloudTrail correlation and verification evidence as release
  artifacts.
- Prove through an IAM denial test that agent and CI identities cannot call the
  production write APIs directly.

Exit gate: a recorded staging demonstration proves both the permitted path and
the denied bypass path. Fake SDK tests alone do not satisfy this gate.

### P1. Replace demo facts with trusted context

Primary issues: #9, #12, and #13.

CI success, dependency health, service metadata, approver roles, and delegation
must come from typed, authenticated sources with revision, freshness, and
provenance. `MISSING` and `INCONCLUSIVE` evidence never become `PASS`.

Exit gate: request-body assertions and UI projection rows cannot independently
authorize a Production Write.

### P1. Operate policy and complete integration

Primary issues: #10 and #11.

- Distribute signed Policy Bundles and report Runner-applied bundle hashes.
- Support shadow evaluation, staged rollout, rollback, and decision-difference
  audit.
- Add reference integration for existing CI/CD and Argo CD without creating a
  second orchestration engine.
- Verify that REST, CLI, and MCP inputs resolve to the same canonical plan and
  authorization path.

Only after the P0 gates pass should work resume on additional adapters, richer
UI, fleet analytics, SaaS features, or broader 200-service optimization.

## Definition of a sellable ECS MVP

The ECS MVP is complete only when all of the following are demonstrated:

1. An agent can request only a typed logical action and never submits an ARN,
   role, command, URL, or generic AWS payload.
2. A valid human or policy authorization is bound into a short-lived signed
   Grant.
3. The Runner independently verifies the Grant and local policy, then reserves
   the nonce before acquiring credentials.
4. STS credentials exist only inside the Runner process and are scoped by
   customer-managed IAM.
5. The Runner performs no more than one provider write for the same authorized
   action, including after timeout, redelivery, or restart.
6. Success requires observed ECS rollout and health, not merely an API or
   workflow success response.
7. The Runner returns a signed Result and signed Evidence that the Control Plane
   verifies before changing durable workflow state.
8. Agent/CI direct access to the same production write API is denied by IAM.
9. Deploy, rejection, timeout, reconnect, reconcile, and rollback evidence is
   preserved in an append-only audit trail.
10. Installation, key rotation, credential isolation, recovery, freeze, drain,
    and break-glass procedures have been exercised from the runbook.

## Work that is deliberately deferred

Until the ECS MVP gates pass, do not prioritize:

- another general-purpose workflow engine;
- arbitrary shell, arbitrary HTTP, or generic cloud actions;
- multiple new deployment providers;
- autonomous planning as an authorization source;
- UI polish, marketplace integrations, billing, or broad SaaS analytics;
- performance work justified only by the 200-service target rather than a
  measured bottleneck.

Support for 200 services remains a design constraint, not the next milestone.
Runner groups should scale by trust boundary/account/environment rather than by
creating one Runner per service.

## Verification policy

Run local checks for the affected boundary, including negative tests and race
checks where relevant. Run the complete CI suite once a coherent change is ready;
do not repeatedly run unrelated local suites. Follow the short common workflow
in [agent operations](agent-operations/README.md). Integration tests that
need PostgreSQL, Temporal, or AWS are reported separately. A skipped external
test is unverified, never passed. Production-readiness claims require preserved
evidence from the dedicated staging environment.
