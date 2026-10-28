# Authorization lifetime scenarios and evidence

See [ADR-0011](../adr/0011-authorization-and-compensation-lifetimes.md).
The implemented path stops when original forward authority expires, including
rollback. A 30m Contract deadline does not extend a 15m Plan or a shorter Context.
No automatic successor or post-expiry compensation permission is implemented.

| Scenario | Expected behavior | Local regression evidence |
|---|---|---|
| Approval at +16m for 15m Plan | Explicit stop, no deploy; old expiry unchanged | `TestAuthorizationLifetimeStopsWithoutExtendingPlan/late-approval` |
| Two observations of 8m each | Both Writes before +15m, last observation completes at +16m | same test `/multi-observation` |
| First observation 16m, later forward step | First result retained, no later Write | same test `/later-step` |
| Final observation 16m | Final read-only result can complete Run | same test `/last-observation` |
| Failed observation after Plan expiry | Explicit reauthorization stop, no rollback | same test `/failed-observation` |
| Large signed receipt, modified epoch, exact deadline with clock skew | Untampered payload verifies; tampering/expired deadline refused | `TestV2LargeSignedPayloadAndStrictDeadline` |
| Valid compensation inside all deadlines | Source-bound Grant and independent manifest | `TestPostgresCompensationAdmission` |
| Expired Plan/approval/compensation, revoked approval, unknown source/target | No new Grant/Write | application and Runner compensation tests |
| Same operation replay or second permission for same source | Restore original; no second compensation Write | Runner compensation tests |
| Concurrent reservations and DB reopen | Exactly one source consumption; restart does not reset it | `TestCompensationConsumptionSurvivesRestart` |
| Changed signed Context or stale policy | Replan/reapprove; no inherited permission | signed source admission and policy Grant admission tests |
| All authority expired, known signed recovery result arrives | Preserve authenticated truth, no new Write | `TestRecoveryReportsAfterExpiryAndRejectsConflicts` |
| Existing Temporal histories | Old version marker preserves command sequence | existing Workflow replay suite |

Workflow tests use virtual time and simulated executor boundaries. Application
and Runner tests use fixture providers and explicit Write/credential counters.
`scripts/test-signed-observation.sh` owns a real PostgreSQL container and verifies
admission plus durable fences across pool reopen. CI's PostgreSQL/Temporal target
also exercises process recovery. These are not AWS/Datadog staging results.

A new Plan or longer interval requires its own approval and manifest. Operator
restart procedure is [authorization expiry](../runbooks/authorization-expiry.md).
