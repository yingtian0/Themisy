# ADR-0019: Optional operator pin for Grant trust bundles

- Status: Proposed
- Date: 2026-10-23
- Related: Issue #4, ADR-0006

An otherwise valid old bundle can restore a revoked key when only the bundle
file is reverted. Add an optional SHA-256 exact-byte pin in Runner-local operator
configuration. Check every fresh lookup and startup, before key resolution.
Reject mismatches without cache fallback; do not let the bundle select its own
pin. Existing unpinned configurations retain their current behavior.

A timestamp floor alone cannot distinguish two bundles with equal issuance time.
An automatic mutable high-water database would add a new durable owner; defer it.
This additive local configuration changes no signed protocol or key encoding.
Protected configuration and bundle deployment must be coordinated; mismatches
stop writes. Reverting both pin and bundle is outside this limited protection.
This is not a signed distribution mechanism, live KMS proof or production gate.
The ADR remains Proposed pending maintainer review.

Tests cover rotation/revocation, old-bundle restoration after resolver restart,
startup refusal, and Runner credential/adapter call counts remaining zero on a
pin mismatch.
