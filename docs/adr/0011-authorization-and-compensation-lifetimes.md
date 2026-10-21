# ADR-0011: Separate review freshness, execution and compensation lifetimes

- Status: Proposed; protocol/state-machine implementation is not enabled
- Date: 2026-10-18
- Scope: Issue #51; depends on final operation policy in PR #56

## Context

Current defaults combine a 15-minute Plan lifetime, a 24-hour approval wait, and
five-minute Grants capped by the original Plan expiry. Observation can outlast
that Plan, after which the next deploy or rollback Grant is refused. Refusing
expired authority is correct; presenting approval as sufficient for a later
Write is misleading. Blanket TTL increases would enlarge the Write window.

Production Plans are immutable. Plan content hashes exclude generated times;
matching hashes alone therefore cannot authorize a new validity interval.
Runner Journal ownership, Control Plane delivery leases, approval validity and
provider operation lifetime are distinct concepts.

## Proposed decision

Use UTC instants and half-open validity intervals: authority is valid when
`issued_at <= now < valid_until`; equality at the deadline refuses a new Write.
Inject clocks at decision boundaries and record the relevant source revisions
and time bounds. Clock-skew allowance for verifying signatures must not extend
an independently expired approval or compensation deadline.

| Object | Meaning / origin | Expiry consequence |
|---|---|---|
| Immutable Plan | Reviewed forward-change content; created after fresh Context collection; current default review freshness stays 15m | No new forward Write. New Plan and approval required; never edit ExpiresAt in place |
| Context authorization snapshot | Authenticated observations, provenance, revision and per-fact freshness; produced by trusted typed readers | Recollect/re-evaluate before a new Write. A changed safety fact invalidates its authorization; matching hashes do not extend its validity |
| Approval | Decision for an exact Plan/Context authorization epoch and reviewed time bounds | Effective forward validity is the minimum of the approval deadline and Plan/Context freshness. A 24h notification/wait maximum is not 24h Write permission |
| Action Grant | One short-lived typed operation, minted only after final policy evaluation | Keep configured 5m default and <=15m ceiling; cap at the effective forward or compensation authority. Expiry/lease loss never permits redispatch |
| Compensation authorization | Explicit reviewed permission for one source operation and fixed predecessor/rollback target, minted before starting that source Write | Only that rollback, within its own bounded period; cannot authorize another forward deployment or arbitrary target |
| Delivery/reconcile lease | Ownership of delivery or read-only recovery work | A replacement owner may deliver the same operation or read-only reconcile; the lease creates no Write authority |

Do not silently carry an approval across a changed Plan, Context revision, time
bound or authorization epoch. Even if PlanHash happens to match, extending the
interval requires a fresh approval. Preserve previous approvals and decisions
in the audit trail.

### Forward changes and long observation

At approval submission and every new Write, verify Plan, Context, Policy,
approval and identity/delegation freshness. On expiry record
`REAUTHORIZATION_REQUIRED` with the expired object and actual deadline. Keep
the old Run/Plan immutable and stop undispatched steps.

Replanning produces a new Plan and linked successor Run containing only the
remaining intended changes. Any dependency carried from the predecessor must
be independently reobserved as trusted runtime evidence; a previous UI success
row is not authorization. The new Run needs its own approval/manifest. Duplicate
successor creation uses an explicit predecessor/epoch key and must not repeat an
already dispatched or UNKNOWN step. A successor cannot be created as an excuse
to retry an unresolved source Write.

Observation started by an already authorized Write can finish after the old
Plan expires. Its signed result may be recorded and verified; this does not
extend the old Plan or enable the next Write. A multi-step release can therefore
finish after 15m when its last Write was authorized before expiry; a later
forward Write requires a new authorization segment.

### Separate compensation permission

Before source dispatch, require a reviewed compensation authorization if
automatic rollback is advertised. It binds tenant, Runner group, originating
Run/step/Grant, PlanHash, approval epoch, service/environment, the source artifact,
one approved predecessor artifact/target mapping, capability, maximum use count
(one), and a non-extendable compensation period. The customer approval authority
signs its independent manifest; the Control Plane cannot invent a predecessor
from a response or change the Runner-local target mapping.

The period starts at the durable source dispatch authorization time, before
credentials/Write, not at response reception, later failure detection, recovery
or retry. An earlier start conservatively bounds response-loss cases. The
deadline is the minimum of Contract rollback deadline and the explicitly
approved maximum; no new numeric default overrides a shorter Contract.

At rollback time, reobserve the source/external state and obtain a fresh typed
Context for the specific compensating action. Evaluate rollback independently.
Mint a short Grant capped at compensation expiry and current authority/freshness.
The expired forward Plan may identify the source but supplies no new forward
authority. Unknown source outcome, unknown predecessor, revoked compensation
approval, unsafe current state or deadline expiry means no new rollback Write;
continue read-only reconciliation and escalate to a human.

Automatic rollback must not remain enabled for an operation whose compensation
permission cannot be provisioned and independently verified. Manual recovery is
a separately authenticated and approved operation, never a bypass flag or an
arbitrary Shell/Cloud API.

### Protocol and durable state

Do not reinterpret v1alpha1 fields or infer compensation permission from a
deploy Grant. A future explicitly versioned protocol/manifest introduces signed:

- `authorization_epoch`: immutable ID/digest binding PlanHash, original time
  bounds, Context revision/hash and approval decision IDs;
- `context_valid_until` and approval validity binding for the specific action;
- `source_grant_id`, approved predecessor digest/mapping revision and
  `compensation_valid_until` for compensation;
- an operation-specific action/Context hash, with rollback distinct from deploy.

Persist authorization epoch/decision, source dispatch time, compensation
permission/consumption, and predecessor/successor relation using versioned
migrations. Atomically create the source reservation/dispatch audit; a missing
commit cannot authorize a Write. Runner shared Journal enforces one Write per
operation; the Control Plane must not duplicate that ownership authority.

Temporal owns waiting, stop/replan and compensation progression. Proposed
states/events are `REAUTHORIZATION_REQUIRED`, `COMPENSATION_UNAVAILABLE` and
`COMPENSATION_EXPIRED`; they are not currently implemented public enum values.
Add state/command changes behind Temporal version markers and replay real
v1alpha1 histories before enabling the new path. New database projection states
must not directly issue credentials or override Journal truth.

### Read-only recovery after all expiries

Accept authenticated signed observations for an already known operation after
its Grant, Plan or lease expiry, under existing recovery signature/hash/tenant
checks. Reconciliation may converge journal/dispatch truth and record evidence.
It cannot consume a fresh Write authorization, renew an epoch, reset a nonce or
directly mark a Run successful without runtime verification. Workflow convergence
remains owned by #38. Keep key revocation/result-trust policy distinct from
authorization expiry; expiry is not a reason to drop recovery evidence.

## Alternatives

- Extend every TTL: rejected; merges review freshness with long-running work.
- Re-sign the same Plan with later ExpiresAt: rejected; silently reuses approval.
- Always allow rollback after failure: rejected; ignores target, deadline and
  source ambiguity.
- Reuse dispatch lease as compensation permission: rejected; delivery ownership
  is not a customer authorization boundary.
- Stop read-only recovery at Grant expiry: rejected; strands unknown writes.

## Migration and current limitations

Until the versioned protocol/state machine is implemented and accepted,
v1alpha1 remains conservative: no new Grant after original Plan expiry,
including rollback. Do not claim that this ADR enables long-lived compensation.
Old Runs/Plans/Manifests have no inferred new authorization epoch. Freeze/drain
old issuers, inventory outstanding/UNKNOWN writes, fence unsupported manifests,
and reconcile them without rewriting history. Create new Plans/approvals for
remaining changes only after source outcomes are known. No in-place production
Plan edits or SQL status fixes are allowed.

Rollout order: accepted design; migrations and strict protocol decoding;
customer manifest provisioning/Runner verification; application expiry reasons
and issuance; versioned Workflow transitions; API/UI explanations; staged
compatibility/replay and failure tests. Reject new versions on old Runners rather
than falling back to v1alpha1 semantics.

## Acceptance and remaining work

The [clock-injected scenario specification](../design/authorization-lifecycle-scenarios.md)
defines boundaries and expected Write counts. This PR provides design and
operator guidance only. No new protocol field, enum, timer, expiry extension,
compensation permission or production behavior is enabled. Issue #51 stays open
until implementation, migration, replay and connection scenarios pass.
