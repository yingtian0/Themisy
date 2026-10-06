# Threat model

## Status

This is the baseline threat model for the target OSS v1.0 architecture. It must be revised when a packet changes a Trust Boundary, protocol, credential path, persistence authority, adapter, or deployment model. The repository now implements a typed GitHub Actions dispatch boundary and Datadog evidence classification, but it has not completed a live dedicated-staging deploy and rollback exercise.

## Security objectives

1. An unauthorized, stale, modified, duplicated, or ambiguous request causes no new external write.
2. A Control Plane or AI-agent compromise does not expose production credentials or enable arbitrary provider operations.
3. A valid action remains bound to its requester, delegated agent, plan, target, policy, approval, artifact, Runner, nonce, and expiry.
4. The same idempotency key causes at most one external write.
5. Decisions, evidence, and actions remain attributable and tamper-evident.
6. Missing evidence, failed compensation, or loss of an authority stops unsafe downstream progress.

## Assets

- production credentials and workload identities;
- signing keys and trust roots for Action Grants, Policy Bundles, results, and releases;
- requester, CI, delegated-agent, Runner, and approver identities;
- Service Contracts, Release Profiles, plans, policy bundles, and approvals;
- workflow state, idempotency records, nonces, execution journals, and external IDs;
- audit events, evidence, tenant configuration, and data-retention controls;
- availability of safe stop, reconciliation, rollback, and escalation paths.

## Adversaries and failure actors

- an external attacker controlling a client or stolen user session;
- a malicious or prompt-injected AI agent acting within or beyond delegation;
- an insider attempting to bypass separation of duties;
- a compromised managed Control Plane or its operator credentials;
- a compromised Runner host, adapter, dependency, or release artifact;
- a confused or buggy external API producing timeouts, duplicates, or reordered events;
- accidental misconfiguration, stale context, unavailable policy, database failure, or metrics loss.

## Trust Boundaries and assumptions

The client, agent output, webhook payloads, request-body claims, provider timeout responses, and raw metrics are untrusted. The Control Plane and Runner are separate compromise domains. The Runner owns final enforcement and production credential access, but it is not assumed immune to compromise; it uses workload identity, least privilege, signed inputs, an allowlist, and local journaling to limit blast radius.

Action Grants and Policy Bundles use canonical Ed25519 signatures as defined in ADR-0006. Production private keys remain behind a KMS/HSM signer interface and are never stored in this repository or Runner configuration. OIDC proofs accept only configured EdDSA or RS256 trust keys. Key discovery, rotation, and revocation remain deployment responsibilities and fail closed when trust material is unavailable. A valid signature proves origin and integrity, not sufficient authorization; all local Runner checks remain mandatory.

## Threats and controls

### Prompt injection and malicious agent output

**Scenario:** Instructions in source code, issues, logs, runbooks, or external pages persuade an agent to alter a target, invent approval, request a generic API call, or disclose a credential.

**Controls:** Natural language is converted to a typed intent and treated as untrusted. The server derives identity and role facts, validates delegation, pins plan inputs, evaluates deterministic policy, and exposes only typed high-level actions. The agent receives neither production credentials nor arbitrary shell/HTTP/cloud tools. The Runner independently validates the Action Grant and local policy.

**Required tests:** Unsupported action fields are rejected; requester-supplied roles or scopes do not authorize a write; a changed target or artifact invalidates the plan or grant.

### Action Grant tampering

**Scenario:** An attacker changes the target, artifact, action, hashes, approval proof, audience, or expiry after a grant is issued.

**Controls:** A canonical representation of every authorization-relevant field is signed. The Runner verifies signature, issuer, audience, target, hashes, subject proof, approval, and expiry before policy or credential access. Unknown fields and version mismatches fail closed. Algorithm selection and key identifiers are explicit, not attacker-controlled fallbacks.

**Required tests:** Single-bit or field-level mutation of every bound field is rejected and invokes neither Credential Broker nor adapter.

**Implemented boundary:** `themisy.protocol/v1alpha1` uses strict JSON decoding and a canonical struct representation. Runner verification fixes the order of protocol, signature/issuer/audience, expiry/replay, verified subject, trusted delegation, pinned context, approval, capability, local OPA, journal reservation, credential acquisition, and typed adapter dispatch.

### Action Grant replay

**Scenario:** A valid captured grant is sent repeatedly, to another Runner, or after its intended execution completed.

**Controls:** Grants have a unique nonce, short expiry, Runner/tenant audience, step identity, and stable idempotency key. The Runner atomically records nonce consumption and execution intent before obtaining a credential. Repeated delivery returns the recorded result or reconciliation state without another write. Nonce state survives restart.

**Required tests:** Concurrent duplicate delivery, replay after restart, replay after expiry, and cross-Runner or cross-tenant replay produce no additional adapter write.

**Implemented boundary:** The local journal reserves both `(tenant, runner_group, nonce)` and `(tenant, runner_group, idempotency_key)` before credential acquisition. Reserved or unknown records require reconciliation and are never blindly dispatched again.

### Credential theft

**Scenario:** An agent, Control Plane service, log collector, malicious adapter, or compromised host obtains a production credential and uses it outside the approved action.

**Controls:** Production credentials never enter the Control Plane or Action Grant. The Runner obtains short-lived, least-privileged credentials only after all checks and passes them through a narrow adapter boundary. Logs, audit, evidence, errors, and traces redact secrets. Egress is allowlisted where possible. Runner identities and credential issuance are auditable and revocable.

**Residual risk:** A fully compromised Runner or adapter may use credentials available during their lifetime. Isolation, short lifetimes, provider-side scope, hardened packaging, signed updates, and anomaly detection reduce but do not eliminate this risk.

### Managed SaaS or Control Plane compromise

**Scenario:** An attacker controls the Control Plane, its database, an operator account, or grant-signing service and attempts arbitrary production writes or audit suppression.

**Controls:** The Control Plane holds no production credential. The Runner accepts a closed typed action set, checks customer/Runner audience and target allowlists, evaluates an immutable mandatory baseline plus a pinned signed policy bundle locally, and requires customer identity and approval proofs. Grant signing, audit integrity, tenant encryption, and operator access use separate roles and keys. A failure to persist required audit state prevents grant issuance.

**Residual risk:** A compromised signer that can satisfy all customer proof and local policy requirements may issue an apparently valid permitted action. Key isolation, quorum or customer co-signing choices, transparency/audit, rapid revocation, and customer-controlled policy are protocol design requirements, not solved by network separation alone.

### Duplicate or reordered webhooks and commands

**Scenario:** CI/CD, runtime, or user events are delivered more than once or out of order, causing duplicate workflow transitions or external writes.

**Controls:** Verify webhook signatures when supported, store provider event IDs with uniqueness constraints, apply optimistic state versions, and make commands idempotent. External actions use a stable `(tenant, adapter, idempotency_key)` uniqueness boundary. A timeout or unknown result is reconciled using the external execution ID before retry.

**Required tests:** Duplicate start, approval, cancellation, provider event, and grant delivery are harmless; an older event cannot move state backward or reopen a completed step.

**Implemented adapter boundary:** GitHub repository, deploy workflow, rollback workflow, and ref are Runner-owned allow-list entries. The adapter sends only fixed `themisy_*` inputs and requires an external workflow run ID. Ambiguous POST responses are recorded as unknown and are never blindly retried. Reconciliation searches only configured workflows for the exact hashed idempotency correlation in the workflow run title.

### Missing, delayed, or manipulated metrics

**Scenario:** The observability source is unavailable, returns an empty series, supplies stale data, or is manipulated so an unhealthy deployment appears safe.

**Controls:** Verification has four outcomes: `PASS`, `FAIL`, `INCONCLUSIVE`, and `MISSING`. Evidence records source, query hash, observation window, timestamps, values, thresholds, adapter version, and integrity hash. Missing or inconclusive evidence extends observation, stops, requests accountable approval where policy permits, or rolls back; it never becomes success by default. Independent checks are required for critical profiles.

**Required tests:** Empty, stale, partial, timeout, malformed, and contradictory results do not advance a required verification gate.

**Implemented evidence boundary:** Datadog sites are selected from a closed hostname set and queries come from Runner configuration. The adapter classifies threshold satisfaction, breach, no series, and unavailable or stale results as `PASS`, `FAIL`, `MISSING`, and `INCONCLUSIVE`. Evidence persists a query hash and bounded numeric metadata; raw query text, logs, and credentials are omitted.

### Stale or malicious service context

**Scenario:** Contracts, catalog data, capabilities, dependency information, or deployment state change after planning.

**Controls:** Plans pin source revision and Contract/Profile/Policy/context hashes with an expiry. Safety-relevant changes invalidate the plan and approvals. Dynamic state is refreshed at dispatch, and unresolved references or capabilities fail planning.

### Policy bypass or downgrade

**Scenario:** A team policy weakens mandatory policy, the Control Plane and Runner use different bundles, or policy is unavailable or expired.

**Controls:** Policy hierarchy is monotonic with a Runner-embedded mandatory baseline. Bundles are versioned, hashed, signed, tested, and pinned to the plan and grant. The Runner is the final enforcement point. Mismatch, invalid signature, missing bundle, or expiry prevents a new write.

Policy modules contribute deny reasons across platform mandatory, environment, team, and service layers. The final decision allows only when the union of deny reasons is empty, so a lower layer cannot erase a mandatory denial.

### Approval forgery and separation-of-duties bypass

**Scenario:** A requester self-approves, supplies fake roles, reuses an old approval, or changes the plan after approval.

**Controls:** Approver identity and roles are resolved from trusted identity sources. Approval records bind roles, quorum, separation requirements, plan and policy hashes, evidence snapshot, and expiry. Input, role, risk, artifact, contract, or plan changes revoke approval.

### Audit loss or tampering

**Scenario:** A write begins when audit storage is unavailable, or an operator later alters decision history.

**Controls:** The state transition and outbox/audit intent are durably committed before grant issuance. Decision and evidence history is append-only, integrity protected, access controlled, and exportable. Signing keys for actions, audit integrity, and tenant data encryption are separated. Projection data can be rebuilt and is not the authorization authority.

### Dependency and rollback safety failure

**Scenario:** A downstream step starts early, a rollback is unsupported or fails, or compensation itself produces an unknown state.

**Controls:** Typed dependencies and state transitions gate eligibility. Rollback capability and deadline are known at planning time. Compensation is an independent authorized execution followed by verification. Failure or unknown compensation state opens a stop/circuit-breaker path and escalates; no downstream step starts.

Release Profiles pin one of `automatic`, `approval-required`, `manual-only`, or `unsupported`. Only `automatic` starts compensation without another decision. Rollback dispatch receives its own idempotency key, execution record, and external ID. The workflow verifies the rollback result; any non-PASS result escalates and leaves downstream steps cancelled.

### Supply-chain compromise

**Scenario:** A dependency, adapter, container, policy bundle, Runner update, or release artifact is replaced with malicious content.

**Controls:** Pin dependencies and artifact digests, minimize dependencies and privileges, generate an SBOM, publish provenance, sign release and policy artifacts, verify before installation, scan continuously, and roll updates out in stages. Runner auto-update must never execute an unsigned artifact.

### Denial of service and resource exhaustion

**Scenario:** A client floods plans, workflows, approvals, verification queries, or Runner work queues, delaying safe operations.

**Controls:** Tenant and actor rate limits, bounded plan size, workflow quotas, multi-layer concurrency budgets, Runner capacity, backpressure, timeouts, and circuit breakers. Emergency freeze and human break-glass are independent; break-glass credentials are unavailable to agents and the normal Runner path.

## Required fail-closed behavior

| Failure | Required result |
|---|---|
| identity or delegation cannot be verified | no plan execution or grant |
| policy missing, invalid, mismatched, or expired | no new write |
| context stale and cannot be refreshed | invalidate plan; no new write |
| audit durability unavailable | no grant and no new write |
| Control Plane disconnected | Runner starts no new production write |
| provider write response unknown | reconcile; do not blindly retry |
| metrics missing or inconclusive | do not pass verification |
| duplicate webhook or command | deduplicate; do not repeat side effect |
| rollback fails or becomes unknown | stop downstream and escalate |
| tenant or Runner audience mismatch | reject grant |

## Security verification baseline

- Unit and property tests cover canonicalization, state transitions, policy hierarchy, grant mutation/expiry/replay, idempotency, and evidence classification.
- Integration tests cover workflow replay, database/outbox atomicity, policy bundle rollout, Runner disconnect/reconnect, and external timeout reconciliation.
- Scenario tests cover Control Plane compromise assumptions, missing metrics, failed rollback, duplicate delivery, and cross-tenant isolation.
- Fuzz targets include parsers and signed canonical inputs before release candidate.
- Release gates require no known Critical or High findings, a published SBOM, signed artifacts, and a practiced vulnerability-response process.

## Out of scope for this baseline

### Shared Runner state and independent manifests

- A forged or re-signed Grant is insufficient to change a separately pinned
  Plan, target, action or approval binding. The manifest approval key and file
  provisioning path must not be available to the Grant dispatcher.
- A stale replica cannot reinstall an older plan revision. Activating a new
  revision fences old reservations at dispatch admission. Already-admitted
  provider calls cannot be recalled by a database change.
- Process crashes, delivery lease expiry and provider absence never authorize
  another write. Recovery claims permit reads and CAS updates, not re-dispatch.
- PostgreSQL loss fails closed before credentials/new writes. Restoring an old
  snapshot without recovering later reservations is unsafe; all replicas must
  remain frozen pending WAL/history reconciliation.
- Recovery reports are authenticated to tenant/group and compared with the
  original canonical Grant hash. Terminal discrepancies are audited rather
  than overwritten. This transport does not replace Result signatures (#9).
- A compromised Runner or customer journal administrator is inside the trusted
  computing base. Use least-privilege runtime roles, append-only audit triggers,
  separate migration identities, TLS and protected backups.

## Remaining release boundaries

This document does not select a signature format, key-management product, identity-provider protocol profile, data-retention period, or managed-service availability SLO. Those choices require packet-specific design, implementation evidence, and ADRs. Air-gapped deployment and full enterprise multi-tenancy are outside OSS v1.0 scope.
