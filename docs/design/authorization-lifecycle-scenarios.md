# Authorization lifetime scenario specification

Companion to [proposed ADR-0011](../adr/0011-authorization-and-compensation-lifetimes.md).
These are implementation test oracles, **not executed test results**. Simulated
time/provider tests, real PostgreSQL/Temporal and live AWS must be reported
separately. Fixed origin: `2026-10-18T00:00:00Z`; forward Plan expires at +15m,
Grant TTL 5m, explicitly approved compensation ends at source dispatch +30m.

| Scenario | Clock / trigger | Required outcome and new Write count |
|---|---|---|
| Fast approval | approve +2m, deploy +3m | New forward Grant expires no later than +8m; exactly one deploy Write |
| Late approval | approve +16m while displayed wait window still open | Explicit reauthorization stop; no new Grant/Write. New Plan and epoch/approval required |
| Boundary | dispatch exactly +15m | Expired; zero Write. +14m59.999999999s is valid only if all other checks pass |
| Long observation | first deploy +1m, observe until +6m; second deploy +7m, observe until +20m | Both writes authorized before expiry; observation/result at +20m can finish. Exactly two deploy Writes, no renewed authority |
| Later forward step | try third deploy +16m | Stop old segment, no third Write. New Plan/successor Run requires fresh dependency evidence and approval |
| Changed Context | same intent, changed artifact/Contract/Policy/CI/health/source revision before Write | Invalidate epoch; no Write until replanned/reapproved. Reusing same PlanHash is insufficient |
| Changed time bounds only | content hash unchanged, ExpiresAt increased | Old approval not transferable; zero new Write |
| Inside compensation window | source dispatch +1m, failure +17m, compensation deadline +31m; fresh rollback Context at +18m | Future versioned compensation path may issue one rollback Grant capped at +23m. Current v1alpha1 refuses because original Plan expired; do not mark this current behavior PASS |
| Compensation boundary | rollback at exactly +31m | `COMPENSATION_EXPIRED`, zero rollback Write; reconcile/read-only and escalate |
| No predecessor | source outcome known, approved predecessor digest/mapping absent | `COMPENSATION_UNAVAILABLE`, zero rollback Write |
| Unknown source | response lost after source Write | No rollback/new deploy until read-only source reconciliation resolves outcome; lease expiry creates no additional Write |
| Revocation | revoke forward/compensation approval before Write | Zero new Write; supported manifest fencing controls outstanding signed Grants |
| Lost completion | source success commit then Worker crash | Restore exact operation/context; no second Write (#49). Recovered result does not start another step without fresh authority |
| All authority expired | signed UNKNOWN→resolved report arrives after +1h | Authenticate and record existing-operation truth; no new Grant/credential Write, no direct Run success assignment (#38) |
| Duplicate reapproval/successor | duplicate signal/request across Worker restart | One epoch/successor and at most one reservation per logical operation; no nonce/key reset |
| Old history | replay pre-lifecycle-change completed/waiting/UNKNOWN histories | No new commands on old version path; no old object silently gains compensation authority |

For each implemented scenario, record source commit, injected times, source
revisions, Plan/approval/Grant/manifest IDs, policy decisions, journal reservation,
credential/adapter call counters, result/audit and final Workflow state. Use
synthetic IDs and redact credentials. Evidence retention must not depend on the
expired authorization object remaining valid.

Current baseline evidence to retain: `TestRecoveryReportsAfterExpiryAndRejectsConflicts`
in `internal/transport/recovery_test.go` accepts authenticated recovery after
expiry and rejects conflicts. It does not prove new lifecycle transitions or
cross-component Run convergence. Existing Runner/Grant expiry tests do not imply
that the future compensation window is implemented.
