# Production identity setup

The development example is not a production trust configuration. Production
uses verified bearer OIDC at every REST/UI/MCP request and an independently
signed organization Directory. See [ADR 0025](../adr/0025-production-identity-directory.md).

## Control Plane and Worker

Add the following to an otherwise complete configuration. Replace all values
with customer-controlled settings; do not put tokens in YAML.

```yaml
identity:
  mode: oidc
  tenant_id: tenant-a
  issuer: https://idp.example.invalid
  audience: themisy-tenant-a
  discovery_url: https://idp.example.invalid/.well-known/openid-configuration
  jwks_cache_ttl: 5m
  tenant_claim: organization
  role_claim: groups
  subject_type_claim: subject_type
  role_mappings:
    release-team: [deployer]
    operations: [operator, sre]
  directory_file: /run/config/identity-directory.json
  directory_issuer: https://organization.example.invalid
  directory_trust_bundle_file: /run/config/directory-trust.json
  directory_minimum_revision: 1
  attestation_trust_bundle_file: /run/config/control-plane-trust.json
```

The optional subject-type claim must be `user` or `ci`; absent means `user`.
The tenant claim is mandatory. Group values must be an array of strings.
Directory roles intersect the mapped roles; actor headers and request/form/MCP
identity fields cannot grant roles. Deployer membership is needed for planning
and starting; operator/admin/sre is needed for release control. Approvers need
all policy-required roles and must be distinct from the requester.

This configuration supports one tenant per deployment. Runner registrations must
match it. Issuer and audience settings are never selected from caller input.
Use separate configurations, keys, Directory and databases for separate tenants.
The trust bundle format is `themisy.grant-trust/v1`, with issuer/tenant-bound,
active Ed25519 public keys and explicit validity windows. The Directory issuer
key must be independently managed from the Control Plane grant signing key.

## Directory provisioning and revocation

Create unsigned JSON with version `themisy.identity-directory/v1`, `issuer`,
`tenant_id`, monotonically increasing `revision`, `issued_at`, `expires_at`,
`members`, and `delegations`. The whole bundle lasts at most one hour. Members
have `issuer`, `subject`, `enabled` and `roles`. Delegation fields use the existing
Go JSON names (`ID`, `Issuer`, `UserSubject`, `AgentID`, `Actions`,
`ServiceSelectors`, `Environments`, `MaximumRisk`, `ExpiresAt`) plus `tenant_id`.
A delegation must expire no later than its bundle and bind an enabled member.
Only `release.deploy` and `release.rollback` are valid delegated capabilities.

Sign in an independent organization provisioning environment:

```sh
go run ./cmd/identitydirectory -input directory-unsigned.json \
  -trust-bundle /run/config/directory-trust.json \
  -kms-key-id "$DIRECTORY_KMS_KEY_ID" > directory-signed.json
```

The command validates the signed result and writes only public identity/scope
records to stdout. Local fixtures may instead pass `-development-key-file` and
`-development-key-id`; never use that signer for production.

Install the signed file atomically at the configured Control Plane and Runner
paths, owned by the provisioning identity and not group/world writable. The
runtime must not be able to change the file or its parent directory. Do not use
symlinks. Removal/disablement of a member, removal of roles, or removal of a
delegation in a higher revision revokes authority at the next check. Keep
revision assignment serialized in the provisioning system; do not reuse a
revision with different content. A missing, malformed, expired or untrusted
file stops new authorization. Renewal of the bundle does not renew an expired
requester proof.

The DB fence is local to each runtime schema. Provision the minimum revision
from an independent trusted source on a new database; restoring a historical
backup alone cannot prove the latest organization revision. Migrations do not
allow an ordinary downgrade to discard a populated fence.

## Runner

Use `mode: production`, existing mTLS/customer journal/approved manifest/adapter
settings, HTTPS OIDC Discovery, and add:

```yaml
identity:
  issuer: https://idp.example.invalid
  audience: themisy-tenant-a
  discovery_url: https://idp.example.invalid/.well-known/openid-configuration
  jwks_cache_ttl: 5m
  directory_file: /run/config/identity-directory.json
  directory_issuer: https://organization.example.invalid
  directory_trust_bundle_file: /run/config/directory-trust.json
  directory_minimum_revision: 1
```

Do not set the development `trust_key_id`, `trust_key_file` or
`delegations_file`. The existing customer-managed Control Plane Grant trust
bundle also verifies the domain-separated identity receipt. The Runner never
accepts the raw IdP token on this production path.

## HTTP, CLI and UI

Send the OIDC token only in `Authorization: Bearer ...` over HTTPS. For the CLI:

```sh
themisyctl -server https://control.example.invalid -tenant tenant-a \
  -oidc-token-file /run/secrets/oidc-token plan release-intent.json
```

Use a mode-0600 token file. The CLI rereads it and does not follow redirects.
Omit requester identity fields in intents or make them match the authenticated
subject; arbitrary supplied values are rejected. Agent scopes are derived from
verified deployer authority. No token goes into the resulting Plan or Run.

UI requests also need the Authorization header, normally added by a customer
OIDC gateway that removes caller identity headers, forwards only validated
bearer authorization over protected transport, and keeps the existing CSRF
protections. This change does not provide an interactive browser login flow,
session store, or a trusted-header authentication mode for production.

## Upgrade and limits

Stop creation of new production writes, apply Control Plane migration 11 and
Runner migration 005 with the existing migration identities, provision both
trust bundles and a fresh Directory, and start Control Plane/Workers and Runner
with the new settings. Do not run a mixed old/new production write fleet.
Existing raw-proof records are not migrated or renewed. Old approvals without
issuer provenance stop in production; replan and obtain a new approval.

Proof receipts last at most five minutes and no later than the original token.
Long approval waits can expire the requester's authority: the safe result is
stopping for fresh authorization. #51 separately addresses that lifecycle; do
not extend a TTL or copy old approvals. Read-only reconciliation remains valid
for already dispatched operations. JWKS revocation visibility is bounded by
configured cache TTL, not immediate IdP lookup on every request.

Run `make test-signed-observation` for owned PostgreSQL observation/revision-fence
tests, and `go test -race -shuffle=on ./...` for the local suite. These tests use
fixtures, not live cloud accounts or production identity providers.
