# ADR 0025: OIDC admission and independent signed identity Directory

- Status: Proposed
- Date: 2026-10-28
- Scope: #12; does not implement Context collection (#13), Policy distribution (#10), or long-lived compensation (#51)

## Problem and constraints

Development actor headers and unsigned delegation files are not a production
identity source. Persisting the original IdP token in a Run or Action Grant also
violates the secret boundary. Identity revocation must survive process restart
and must be checked again before new writes, without granting an expired write
permission a longer lifetime.

## Decision

Use explicit `identity.mode: oidc` in the Control Plane, including Workers. One
configured tenant/issuer/audience/claim mapping is supported per deployment.
OIDC authentication protects REST, UI and MCP; health and authenticated Runner
transport remain separate. Development headers are a separate development mode
and are rejected with AWS KMS grant signing.

Intersect configured IdP group mappings with an independently signed, short-lived
organization Directory. Read its local protected file for each decision. A
customer-managed provisioning pipeline signs and atomically installs updates;
this change supplies a signing command, not a Directory distribution service.
Persist the greatest accepted revision and its content hash in each Control
Plane and Runner database. Lower revisions and conflicting equal revisions fail
closed. Deleting a membership or delegation in a higher revision revokes it.

Consume the IdP token only in request memory. Exchange it over pinned HTTPS mTLS
with a separately deployed `identityattestor` workload. That workload independently
verifies OIDC, Directory roles and agent delegation and signs with an identity-only
key. The Control Plane/Worker and Runner have only its public trust bundle.
Replace the token before persistence with
a domain-separated Ed25519 verification receipt, valid for at most five minutes
and never beyond the token. Bind issuer, subject, type (user/CI), tenant,
audience, OIDC key fingerprint, request, agent and delegation. The receipt is
not accepted as HTTP authentication. The Runner accepts receipts only in
production; development retains the existing fixture JWT path. Both sides check
current Directory membership and issuer-pinned JWKS trust, using the existing
bounded cache with no stale fallback. Cached key removal is visible at refresh,
not instantaneously; Directory removal is visible at the next decision.

Include requester/issuer/tenant/agent/delegation in new Plan hashes. Store the
verified approver issuer in approval signals and projections, and recheck the
approver's current Directory roles at Grant issuance. Reject old approvals with
no verified issuer in production. Compare pinned and requested risk
conservatively so a lower caller risk cannot widen delegation authority.

Audit identity decisions without tokens or private claims. Audit failure stops
admission. Revalidate after signing and again in the Runner after credential
acquisition, before provider dispatch. New Grants cannot outlive the subject
receipt, delegation, Directory or Plan. Expiry requires a new authorized flow;
there is no automatic renewal and no reuse of approval under a changed Plan.

## Alternatives and consequences

Storing a raw token in the durable workflow would preserve direct IdP proof but
also preserve a bearer credential. An unsigned database membership cache would
make revocation dependent on an untrusted update channel. A signed verification
receipt plus independently provisioned Directory avoids those choices while
requiring explicit trust provisioning and short-lived operation.

The receipt is an independent identity authority's attestation of OIDC verification,
not the original IdP signature on the normalized receipt. The Grant signer has
no identity-attestor signing permission. A proof forged with the Grant key is
rejected, even when the Grant itself is valid. Identity issuer, Directory issuer
and Grant issuer are distinct; workload credentials, KMS policies and public
trust bundles must preserve that separation. Both the Control Plane and Runner
also check the IdP key remains trusted and independently read the signed
Directory. The identity authority has its own durable audit and revision fence;
an outage stops new starts instead of using cached receipts as login authority.

Existing records are not rewritten. Quiesce old production writes, migrate both
schemas, provision trust and Directory, then replan/reapprove. Read-only
reconciliation of already dispatched operations keeps its existing path. The
new migration refuses to erase a populated rollback fence on downgrade.

## Validation

Negative tests cover signature and subject changes, tenant/audience/issuer/key
mismatch, member/delegation/role removal, expired receipts, unavailable Directory,
audit outage and revocation during signing or credential acquisition. A disposable
PostgreSQL fixture checks concurrent revisions and reopening both schema pools.
No live AWS, IdP or Datadog environment is claimed by these tests.
