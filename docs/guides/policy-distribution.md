# Signed Policy rollout

Active Policy distribution is distinct from the offline `policy-candidate`
cache. An object in that cache never authorizes a Run. See
[ADR 0026](../adr/0026-signed-policy-distribution.md).

## Provisioning

Use the [OIDC identity setup](production-identity.md). Give policy maintainers a
mapped `policy-admin` role **and** the same role in the independently signed
Directory. The Grant signer cannot sign customer Policy. Provision a separate
Policy issuer trust bundle using the existing tenant-bound
`themisy.grant-trust/v1` public-key schema, with explicit validity and revocation.
The Control Plane, Workers and Runner read this local trust fresh for decisions.

Control Plane and Worker configuration:

```yaml
policy:
  mode: distributed
  issuer: https://policy.example.invalid
  trust_bundle_file: /run/config/policy-trust.json
  runner_groups: [canary, production]
```

Runner configuration, alongside existing mTLS/identity/manifest/adapter settings:

```yaml
policy:
  issuer: https://policy.example.invalid
  trust_bundle_file: /run/config/policy-trust.json
```

Production Runner rejects the old `bundle_file`, `trust_key_id` and
`trust_key_file` settings. It fetches its assignment through its authenticated
outbound connection and revalidates signature, expiry, compilation and revision
before evaluation, including immediately before a new provider Write. A missing
assignment or failed fetch does not authorize with a cached/default bundle.
Read-only observation/reconciliation of already dispatched operations remains
separate from new Write authorization.

## GitOps and signing

Review unsigned Bundle JSON and Rego in a PR. Preserve layer names
`platform_mandatory`, `environment`, `team`, `service`. The embedded mandatory
baseline is evaluated independently; each populated cumulative prefix is also
evaluated. A later allow cannot cancel an earlier deny or approval role. All
modules compile without network, environment/runtime inspection, random or
wall-clock builtins. Reasons must match the stable Reason Code format.

Run unit/fixture and shadow tests before promotion. Sign the reviewed content in
the independent Policy publishing workload:

```sh
go run ./cmd/policybundle -input reviewed-policy.json -tenant tenant-a \
  -trust-bundle /run/config/policy-trust.json \
  -kms-key-id "$POLICY_KMS_KEY_ID" > signed-policy.json
```

The command verifies the resulting signature and compiles all layers before
writing the signed Bundle. Local fixtures may instead use
`-development-key-file` and `-development-key-id`. Production uses a separate
KMS workload; do not give the Control Plane or Runner its signing permission.

New signatures bind `signing_version: themisy.policy-signature/v2` and a
canonical SHA-256 message with domain separation, retaining the KMS 4-KiB input
bound even for larger sources. Removing/changing the signing version breaks the
signature/hash. The runtime also accepts valid legacy signatures, while still
enforcing its embedded baseline. Deploy the new decoder before registering v2.

## API sequence

All management endpoints require verified OIDC/Directory `policy-admin` authority.
Tenant comes from authentication; it is never supplied as an activation field.
Use HTTPS and protected bearer-token handling, without request/response logging
of credentials.

| Operation | Endpoint and body |
| --- | --- |
| Register reviewed signed bundle | `POST /v1/policy/bundles`, body is the signed Bundle |
| Inspect/reverify bundle | `GET /v1/policy/bundles/{bundle_hash}` |
| Inspect assignment and delivery acknowledgements | `GET /v1/policy/assignments/{runner_group}` |
| Assign to one configured group | `PUT /v1/policy/assignments/{runner_group}`, `{"bundle_hash":"sha256:...","expected_revision":0}` |
| Compare current/candidate | `POST /v1/policy/assignments/{runner_group}/shadow`, `{"candidate_hash":"sha256:...","cases":[...]}` |
| Roll back to previous signed bundle | `POST /v1/policy/assignments/{runner_group}/rollback`, `{"expected_revision":2}` |

Start with the canary group, inspect its authenticated acknowledgement and run
customer staging checks, then assign other groups explicitly. Acknowledgement
means `DELIVERY_VERIFIED_ONLY`; it does not assert that any cloud operation ran
or that a service is healthy. Shadow reports compare the same canonical Input
and expose decisions, reasons and required-role differences. Shadow itself does
not activate a bundle or approve a release.

A new assignment uses expected revision `0`; later updates must provide the
current revision. Conflicting/stale updates return 409. Registration is immutable
by tenant/hash and by tenant/issuer/version; do not reuse a version for different
content. Assignment, acknowledgement and audit changes commit atomically.

Rollback advances the assignment revision and reverifies the previous signed
Bundle and trust. Expired/revoked previous content cannot be reactivated. Status
retains the current revision/hash even if its Bundle is unavailable, so an
administrator can request an explicit recovery. There is no automatic rollback
or fallback on an outage.

## Plans, approval and upgrade

One Run must use the same signed Bundle hash across all selected groups. If
canary and stable groups differ, create separate Runs. Each group's assignment
revision is pinned into the Plan hash, canonical Policy Input, Action Grant and
independent Manifest. New Grant issuance checks the current assignment again
under the database transaction. A rollout, or a rollback to the old hash with a
new revision, invalidates the prior Plan/approval for new Writes. Replan and
reapprove; never copy an old approval onto a new hash/validity period.

Quiesce production writes and upgrade Control Plane/Workers and Runner together.
Apply Control Plane migration 12 and Runner migration 006 using migration
identities. Legacy global prototype `policy_bundles` rows are left intact and
are not promoted into the new scoped registry. Register/reverify reviewed signed
Bundles and assign each group explicitly. Existing Plans without assignment
revision are not silently migrated. The downgrade refuses to discard active
assignment history; Runner revision fences must also be retained across restore.

A new Runner database needs a trusted current provisioning state; historical
backup restoration alone cannot prove that a policy revision is the latest.
The live authenticated fetch and local monotonic fence prevent ordinary replay.

`go test -race -shuffle=on ./...` covers layer precedence, API authorization,
scoped canaries/rollback, old-approval rejection and zero credential/Write calls
on invalid Policy. `make test-signed-observation` uses an owned disposable
PostgreSQL fixture to check restart fences, atomic admission and existing signed
observation flows. CI runs both paths. These are fixture results; real customer
canary health and cloud rollout evidence remain external staging work.
