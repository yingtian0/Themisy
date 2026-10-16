# Signed Result reports

Both `POST /v1/runner/grants/{id}/result` and `/reconcile` require a
`report` object of version `themisy.result-report/v1`. There is no unsigned
compatibility mode. The existing outer `result` must exactly match
`report.result`; recovery's outer `request_hash` must match
`report.request_hash`. Deploy Control Plane and Runner changes together; old
clients fail closed. Keep durable journal Results: the signed reporting envelope
is separate and can be regenerated after restart, peer recovery, or key rotation.

The report contains `version`, reporting `runner_id`, `tenant_id`, `runner_group`,
`request_hash`, `result`, and `signature` (`algorithm`, `key_id`, `value`). The
reporter identifies the authenticated observer and is not necessarily the
original executor. The request hash is the canonical original Grant hash.

Signing bytes are UTF-8 `themisy.result-report/v1`, one NUL byte, and Go's
fixed-field JSON serialization of ResultReport with the signature value empty.
The signature metadata remains present. Result completion time is UTC
RFC3339Nano. Result fields bind protocol version, Grant/run/step, status, reason
code, provider execution ID, completion time and optional EvidenceHash. The legacy
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

This implements Result reporting for issue #9. Optional Result.EvidenceHash binds
post-execution evidence by digest and is independent of the Grant's pinned
pre-execution EvidenceHash. Evidence body signing, evidence collection/runtime
integration and the rest of #9 remain separate work.
