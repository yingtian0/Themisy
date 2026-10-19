# ADR-0010: Local signed Policy candidate catalog

- Status: Proposed
- Date: 2026-10-20
- Related: Issue #10, ADR-0003, ADR-0005, ADR-0006

## Context and constraints

Shadow comparison can review signed candidates, but there is no immutable local
place to retain and retrieve the exact reviewed Bundle. A local artifact cache
must not become a second application database or a policy activation authority.
Registration cannot imply approval, successful deployment or Runner adoption.

## Proposed decision

Provide an opt-in operator-local `CandidateCatalog`, disconnected from the
Control Plane, Runner, Engine and Grant issuance. It stores the complete signed
Bundle under its exact content hash in an existing operator-owned directory.
Hash-addressed retrieval accepts no version aliases or caller-selected paths.
Registration and every retrieval verify configured issuer/key trust, hash,
compatibility, layers, issuance/expiry and offline compilation. Registration
neither signs nor renews an artifact. Existing objects are never overwritten,
including corrupt or expired objects; operators must investigate such failures.

Use a same-directory temporary file, file sync, create-if-absent hard link, then
directory sync. Separate catalog instances can safely race to publish the same
hash. Errors after publication remain errors; retry revalidates the existing
object. Linux/macOS local filesystems with hard links and directory sync are the
initial supported scope. A failed operation can leave a valid visible object;
an abrupt process death can leave a temporary file, never a partial final file.

## Alternatives

- PostgreSQL registry plus rollout records: appropriate for the eventual
  authenticated service, with atomic audit and tenant/Runner-group scoping, but
  not yet designed or connected. This cache does not substitute for it.
- Version-named files and replacement: rejected because a human-readable version
  must not silently rebind a reviewed hash.
- Trust only on registration: rejected because expiry or key removal must take
  effect on later reads.

## Consequences and safety

This is candidate storage only. There is no active pointer, rollout, rollback,
applied acknowledgement, deployment audit or HTTP API. Multiple hashes may carry
the same version; callers must use the exact hash. No policy execution permission
or production trust boundary changes. Existing enforcement remains unchanged.
Registration proves signature/lifetime/compilation only, not behavioral safety,
mandatory-baseline equivalence, review, or permission to apply. The existing
Shadow report is separate and is not a promotion token.

The directory and its ancestor configuration must be operator-controlled; this
is not a multi-user writable service or a hostile-filesystem sandbox. `os.Root`
confines operations, final object symlinks/nonregular files are refused, JSON is
bounded to 2 MiB, and the offline compiler cannot fetch network/runtime facts.
There is no remote fetch/import, automatic cleanup, or automatic repair.
Filesystem contents have no audit authenticity guarantee beyond each Bundle's
signature. Production registry/audit authority remains subject to a separate
reviewed design under ADR-0005.

## Validation and remaining decisions

Tests cover fresh-instance recovery, expiry and trust removal on reads,
concurrent publication, permissions, corruption/hash mismatch, unknown fields,
oversized input, symlinks, invalid signatures/issuers, cancellation and network
builtins refused before publication. Power-loss filesystem durability and
Windows/network-filesystem portability remain unverified. This ADR is proposed;
maintainer acceptance and the full Issue #10 API/rollout acceptance remain open.
