# Signed Result reports

Both `POST /v1/runner/grants/{id}/result` and `/reconcile` require a signed
`report` object. Version `themisy.result-report/v1` supports Results without an
Evidence body; `themisy.result-report/v2` additionally supports an optional
Evidence body. There is no unsigned compatibility mode. The existing outer
`result` must exactly match `report.result`; recovery's outer `request_hash` must match
`report.request_hash`. Deploy Control Plane and Runner changes together; old
clients fail closed. Keep durable journal Results: the signed reporting envelope
is separate and can be regenerated after restart, peer recovery, or key rotation.

The report contains `version`, reporting `runner_id`, `tenant_id`, `runner_group`,
`request_hash`, `result`, and `signature` (`algorithm`, `key_id`, `value`). The
reporter identifies the authenticated observer and is not necessarily the
original executor. The request hash is the canonical original Grant hash.

Signing bytes are UTF-8 report version, one NUL byte, and Go's fixed-field JSON
serialization of ResultReport with the signature value empty. The signature
metadata remains present. Result completion time is UTC RFC3339Nano. Result
fields bind protocol version, Grant/run/step, status, reason code, provider
execution ID, completion time and optional EvidenceHash. The legacy
Result.signature must be empty. Algorithm is Ed25519; signature encoding is raw
base64url. The entire signing payload is bounded to 4096 bytes for AWS KMS.
Successful observations require an external execution ID. Reason codes and IDs
are bounded scalar metadata; arbitrary provider responses, credentials, delivery
tokens and user identity proofs are outside this schema and must never be placed
in these fields.

The Runner signs once after local REJECTED-to-UNKNOWN normalization and signs
journal recovery observations using its current reporting identity/key. Server
verification binds the report to the authenticated Runner and stored Grant
before mutation. Recovery works after the original Grant/lease expiry: current
reporting trust validity is checked, rather than the old observation time.
UNKNOWN never authorizes another provider write.

## Runtime configuration and migration

Configure Runner `result_signing` independently of Grant trust:

```yaml
result_signing:
  provider: aws-kms
  key_id: arn:aws:kms:REGION:ACCOUNT:key/KEY
  aws_region: ap-northeast-1
```

The KMS key must support the existing Ed25519 signing integration. Local
`provider: development` additionally requires `private_key_file` (an explicitly
provisioned base64url Ed25519 seed/private key with owner-only permissions), and
is accepted only in development mode. No default or ephemeral runtime signer is
created. Configuration checks validate provider/key/region or private-key path
without contacting AWS; startup loads the signer.

Each Control Plane `runner_transport.registrations` entry must set
`result_trust_bundle_file`. Provision a separate file per reporting Runner:

```json
{
  "version": "themisy.result-trust/v1",
  "runner_id": "runner-local-1",
  "tenant_id": "tenant-development",
  "runner_group": "local-development",
  "issued_at": "2026-10-16T00:00:00Z",
  "expires_at": "2026-10-17T00:00:00Z",
  "keys": [{
    "key_id": "development-result-key",
    "public_key": "BASE64URL_ED25519_PUBLIC_KEY",
    "status": "active",
    "not_before": "2026-10-16T00:00:00Z",
    "not_after": "2026-10-17T00:00:00Z"
  }]
}
```

This Control Plane-local trust is not a Grant trust bundle. Startup validates it
and each verification rereads the bounded regular file; missing, malformed,
expired, duplicated, unknown or revoked keys fail closed. Replace the file
atomically to rotate/revoke. Overlap old/new active entries while switching the
Runner signer, then revoke the old entry. Recovered Results receive fresh
signatures; their durable semantic identity does not depend on the signing key.

Accepted reports, including identical terminal retries, persist the complete
verified signed envelope in audit `details.signed_report` atomically with dispatch
state. Retries retain the existing terminal state/version while recording the
new report. Rejections record only authenticated identity and stored Grant
metadata, never unverified report data. Audit failure prevents acknowledgement.
The delivery endpoint still requires its original Runner/token even on terminal
retries; registered peers use the recovery endpoint.

In report v1, `result.evidence` is not allowed. Existing v1 reports remain
verifiable when the body is absent. Report v2 may include the typed
post-execution Evidence body in `result.evidence`.

When `result.evidence` is present, it carries the typed post-execution Evidence
body. Its canonical EvidenceHash is recomputed from the body (timestamps are
normalized to UTC) and must equal both `result.evidence_hash` and the body's
`evidence_hash`. The complete body and both hashes are part of the signed report
payload, so changing evidence and recomputing its hash still invalidates the
Runner signature. Omitting the body remains compatible with Results that contain
no post-execution evidence. The report-size bound also applies to this body.

The result EvidenceHash is independent of the Grant's pinned pre-execution
EvidenceHash. The protocol can preserve and authenticate an Evidence body, but
the current deployment path does not collect runtime observations or populate
this field. The new report v2 reader is additive; existing reports use v1 while
new signers emit v2, so deploy Control Plane and Runner changes together.
Runner-side evidence collection, separate runtime/evidence storage,
verification workflow integration, and the remaining #9 acceptance conditions
are still incomplete; this change does not make those paths operational.

The signing helper owns an independent Evidence value and normalizes its times
to UTC before signing. Reusing the caller's original observation cannot mutate
the returned signed report. Cancellation before or during signing returns no
report. Evidence chronology must satisfy `window.to <= observed_at <= completed_at`;
a well-formed hash or signature does not make impossible observation times valid.
This does not require a recovered report to finish within the Grant's execution
lifetime and does not authorize any new write after Grant expiry. Existing v1
reports without Evidence and valid v2 canonical bytes remain compatible.

## Source-bound read-only observations

`release.verify` uses the same authenticated outbound transport but cannot enter
the write dispatcher. Its optional Action fields bind the source Grant ID, nonce,
request hash, idempotency key, operation and original execution, plus an exact
UTC verification window. The source must already be SUCCEEDED in the Runner
journal; see [ADR-0024](../adr/0024-source-bound-runtime-verification.md).

Result-report/v2 now optionally includes `runtime` (including confirmed container
digests) and raw metric `health`, alongside combined `evidence`. The signature
binds every body. The verifier checks the requested source/window and recomputed
hashes/decision before persistence. Old reports omit these fields and retain
their previous canonical encoding. V1 cannot carry the new bodies. Upgrade both
ends before enabling the capability; do not reinterpret old deploy reports as
verified observation receipts. `sampled_at` is the metric timestamp; `observed_at`
is collection time, after the observation window. Query expressions come only
from trusted Runner configuration and are signed with thresholds and values.
