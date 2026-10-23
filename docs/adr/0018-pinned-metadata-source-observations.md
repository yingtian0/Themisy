# ADR-0018: Pinned Contract and Profile source observations

- Status: Proposed
- Date: 2026-10-23
- Related: Issue #13, ADR-0005, ADR-0015, ADR-0017

## Context

Local schema validation and a canonical content hash do not identify who supplied
Service Contract and Release Profile bytes. Request-selected documents or mutable
branch heads cannot independently provide trusted pre-dispatch metadata.

## Proposed decision

Add an opt-in collector using the existing read credential broker and strict
Contract/Profile decoders. Operator configuration maps tenant/service/environment
to a GitHub repository, immutable commit SHA, two repository-relative paths and
maximum fetch age. Requests select only the logical key. Default HTTP composition
uses fixed HTTPS API requests, bounded responses/timeouts and no redirects or
download URL following. No command defaults change.

Verify returned path/type/base64/size/blob identity, then validate document schema,
service/environment binding, selected Profile name and required capabilities.
Record source selectors, schema, blob identity, raw SHA-256, canonical content
hashes and fetch time in one audit event before returning a private snapshot.
Return newly decoded copies for inspection; never restore trust from JSON or
stored audit records. Partial acquisition, provider failure, invalid input,
cancellation, ambiguous audit append and expiry cannot return a usable snapshot.
Freshness is checked again after audit, and must be checked at each later use.

## Alternatives

- Accept caller documents plus hashes: rejected because this proves consistency,
  not source authority.
- Fetch a branch or tag: rejected because a mutable ref can change between reads.
- Follow API download URLs: rejected to keep token-bearing reads on a fixed host.
- Add signed repository-tree and build provenance here: deferred to a separate
  attestation unit; recording hashes must not imply this stronger guarantee.

## Consequences and safety limits

GitHub's authenticated API is the source authority; Git SHA-1 is a provider blob
identifier, not a signature or independent integrity proof. SHA-256 hashes bind
raw and canonical content. GitHub may resolve internal symlinks as file content;
this unit does not attest tree modes. Token scope is operator-enforced. Maximum
age measures observation freshness, not commit recency or latest-branch status.
Operator revision changes require recollection.

Audit stores source/hash metadata without raw documents, credentials or raw
provider/parser errors. An append acknowledgement loss may leave an observation
without returning a snapshot. Audit PASS means validation at capture, not an
execution authorization. Independent maintainer review is still required; this
ADR remains Proposed. Full Context composition, Plan/Grant binding, approval
invalidation and live source staging remain open. No signed protocol, database
schema, Workflow graph or production listener changes.

## Validation

Synthetic token/HTTP fixtures cover immutable source selectors, strict decoding,
wrong target/profile/capabilities, modified blob/size/content, partial acquisition,
provider errors, cancellation, credential expiry, clock reversal, observation
expiry during audit, independent copies and rejected JSON restoration. The
existing fresh PostgreSQL audit fixture verifies persistence/reopen with
reproducible hashes and refused INSERT returning no snapshot. These tests do not
establish live GitHub staging acceptance.

See [composition guide](../security/metadata-context.md).
