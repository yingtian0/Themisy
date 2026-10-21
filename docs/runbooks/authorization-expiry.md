# Handling expired authorization today

Current v1alpha1 Grants are capped by the original Plan's expiry. A 24-hour
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
   successor Run creation or a separate compensation authorization.

[ADR-0011](../adr/0011-authorization-and-compensation-lifetimes.md) proposes the
future separation of review freshness, short Grants and bounded compensation.
It is not an implemented recovery command or permission to use production
credentials. Until #51 is implemented, do not recommend long approval waits or
multi-stage execution that relies on post-expiry rollback.
