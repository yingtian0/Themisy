# Runner-local Action Grant trust bundle v1

This file supplies the Runner's trusted Ed25519 public keys for one Control
Plane issuer and tenant. It is a customer-provisioned local configuration file,
not a network discovery document or an Action Grant field.

```json
{
  "version": "themisy.grant-trust/v1",
  "issuer": "https://control.example",
  "tenant_id": "tenant-1",
  "issued_at": "2026-10-14T00:00:00Z",
  "expires_at": "2026-10-21T00:00:00Z",
  "keys": [
    {
      "key_id": "grant-2026-10",
      "public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
      "status": "active",
      "not_before": "2026-10-14T00:00:00Z",
      "not_after": "2026-10-21T00:00:00Z"
    }
  ]
}
```

The JSON object has exactly these fields. Version must be
`themisy.grant-trust/v1`. The issuer and tenant must equal immutable local
Runner configuration. Timestamps are RFC 3339 UTC values. The bundle must be
currently valid and contain 1–64 entries. Key IDs are unique. Every key is a
32-byte Ed25519 public key encoded as unpadded base64url, with status `active`
or `revoked` and a nonempty validity interval. Public key bytes cannot appear
under more than one ID, including revoked entries.

For rotation, place distinct old and new keys in one bundle during the planned
overlap. The old key can then be revoked or removed in a protected atomic file
replacement. Every verification rereads the file, so a replaced or unavailable
bundle takes effect at the next check. There is no cache fallback.

The bundle is trusted because its path and replacement process are controlled
by the customer. The file is unsigned and does not protect itself against an
administrator restoring an older copy. Use deployment access controls and
audit history for the provisioning path. This protocol does not define KMS/HSM
signing operations, OIDC/JWKS trust, Policy Bundle verification, or cross-host
distribution.

## Optional operator hash pin

Set `control_plane.trust_bundle_hash` to `sha256:` followed by the lowercase
SHA-256 of the exact bundle file bytes. The Runner checks it on startup and on
every key lookup, before resolving any key. Wrong, malformed or old replacement
files are refused without a previous-file cache. A pin without a bundle file is
invalid. Omitting this additive setting preserves the existing refresh behavior.

For rotation, deploy the new bundle bytes and matching pin using protected
operator configuration; temporary mismatch fails closed. Keep the pin in a
separately controlled deployment configuration. Pinning detects restoration of
an old active-key bundle even if its timestamps equal the newer revocation
bundle, including after Runner restart. It is not automatic distribution or
monotonic storage: an operator who reverts both configuration and file can
restore old trust. Exact-byte formatting changes also require a new pin. Live
KMS and operational distribution drills remain external acceptance gates.
