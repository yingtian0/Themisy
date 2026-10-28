# Trusted source admission

Standard OIDC execution requires signed pre-dispatch Context, independently
signed build provenance, distributed Policy and identity admission. Caller
`ci_success`/`dependencies_healthy` flags are not authorization facts. Development
`context.mode: synthetic` remains a separate fixture path.

[ADR 0027](../adr/0027-signed-source-admission.md) describes the boundary. These
instructions implement a Themisy-specific build statement and source receipt;
they do not claim SLSA/Sigstore verification or completed live staging.

## Authorities and credentials

The build workload signs a public statement of the actual deployable artifact.
The customer-side collector has only read credentials for allow-listed GitHub
CI/metadata and Datadog dependency health, its own source-signing permission and
customer audit DB. Control Plane/Workers and Runner have only public source/build
trust and provisioned signed source files. Agent and Grant-signing workloads
must have no build/source signing permission.

Provision separate issuer/tenant-bound public trust bundles using the existing
`themisy.grant-trust/v1` schema, with key validity and revocation. Do not reuse
signing authorities for build provenance, source collection, identity, Policy,
independent approval manifests or Grants. Credentials are never part of the
public envelopes or their audit records.

## Build statement

In the trusted build pipeline create JSON from actual build outputs:

```json
{
  "issuer": "https://build.example.invalid",
  "tenant_id": "tenant-a",
  "service": "payment-api",
  "repository": "customer/application",
  "commit_sha": "<exact lowercase 40-character commit>",
  "workflow": "build.yml",
  "run_id": 123,
  "run_attempt": 1,
  "artifact_digest": "sha256:<actual deployable image digest>",
  "destructive_migration": false,
  "issued_at": "<UTC timestamp>",
  "expires_at": "<UTC timestamp, at most 30 days after issue>"
}
```

Sign under the independent build identity:

```sh
go run ./cmd/buildprovenance -input build-output.json -tenant tenant-a \
  -trust-bundle /run/config/build-trust.json \
  -kms-key-id "$BUILD_KMS_KEY_ID" > build-statement.json
```

The CI workload/KMS trust policy must restrict who can sign each repository's
statements. An Agent must not choose the statement's claimed build output.
Provision this public signed statement to the source collector. GitHub artifact
archive digests remain archive identifiers; they are not substituted for the
statement's actual image digest. Collection binds repository, commit,
workflow/run/attempt and service to both the CI result and build signature.

## Source policy shared by consumers

Use equivalent trusted settings in Control Plane/Workers and Runner, under
`context:`. Paths refer to each workload's independently provisioned local files.
This example must be expanded to the complete referenced Contract/Profile catalog.

```yaml
context:
  mode: signed
  file: /run/config/source-admission.json
  issuer: https://sources.example.invalid
  trust_bundle_file: /run/config/source-trust.json
  build_issuer: https://build.example.invalid
  build_trust_bundle_file: /run/config/build-trust.json
  minimum_revision: 1
  maximum_age: 5m
  dependency_maximum_age: 5m
  targets:
    - service: payment-api
      environment: production
      owner: customer
      repository: application
      workflow: build.yml
      maximum_age: 24h
  metadata:
    - service: payment-api
      environment: production
      owner: customer
      repository: release-config
      revision: "<immutable 40-character config commit>"
      contract_path: contracts/payment-api.yaml
      profile_path: profiles/critical.yaml
      maximum_age: 1h
    - service: identity-api
      environment: production
      owner: customer
      repository: release-config
      revision: "<same reviewed immutable config commit>"
      contract_path: contracts/identity-api.yaml
      profile_path: profiles/standard.yaml
      maximum_age: 1h
```

The source-wide maximum age is positive and at most 15 minutes. Every child
observation must also satisfy its own trusted maximum age. Source-provided
allowances cannot widen consumer configuration. The metadata repository,
revision and paths are operator allowlists, never request parameters. Include
all referenced services and profiles required for the graph; inconsistent,
missing or schema-invalid metadata cannot produce a Plan.

The source envelope owns validated metadata bytes, Git blob identity, source
revision/path, content hash and fetched time. The standard Planner uses that
verified catalog; local prototype files are not the production metadata authority.
`critical` remains above `high`, requires mandatory approval, and requires an
explicit `critical` maximum-risk delegation; it is never normalized downwards.

## Customer-side collector

Use `go run ./cmd/contextcollector -config source-collector.yaml -revision N`.
The revision is serialized, strictly increasing per tenant/source issuer. Do not
reuse a revision with different content, including after an ambiguous publish.

Collector configuration has:

```yaml
mode: production
tenant_id: tenant-a
signing:
  provider: aws-kms
  key_id: source-key-id
  aws_region: ap-northeast-1
github_token_file: /run/secrets/github-read.json
datadog_credential_file: /run/secrets/datadog-read.json
source:
  # The entire signed context configuration above, without the context: wrapper.
  # Each target additionally has:
  # build_statement_file: /run/config/build-statement.json
dependencies:
  - service: payment-api
    environment: production
    services: [identity-api]
health:
  - service: identity-api
    environment: production
    site: datadoghq.com
    expression: "<reviewed bounded dependency metric query>"
    comparator: lte
    threshold: 1
    aggregation: last
    minimum_points: 1
    maximum_age: 5m
    window: 5m
```

Use the exact supported Datadog site/query settings for the customer's account.
Dependency sets must agree with the trusted Contract's complete service set.
No configured dependency set is interpreted as healthy by default. A Contract
that explicitly has no service dependencies needs no dependency health read.

The protected mode-0600 GitHub file contains `token` and `expires_at`; the
Datadog file contains `api_key`, `app_key` and `expires_at`. Keep those files only
in the customer reading environment. Set `THEMISY_CONTEXT_DATABASE_URL` to its
customer Runner-schema audit database, provisioned with Runner migrations.
Never give this process release database credentials or deployment credentials.
Local fixtures may explicitly use `mode: development` and a development signer;
production requires KMS and durable customer audit.

The collector uses the real typed read-only adapters and never calls a deployment
adapter. It atomically publishes the configured `source.file` and prints only
hash/revision/status/reason. Successful **publication**, including a negative
observation, exits successfully: exit code 0 does not mean `PASS`.

Provider failure, partial data, invalid provenance or non-PASS health publishes a
negative signed revision rather than reusing an earlier PASS. Its inspectable
available observations retain their four-value status/reasons. Propagate negative
files atomically to consumers just as positive files. Signing/audit/configuration
failure is an error, not a positive observation. An old positive file remains
usable only within its bounded observation lifetime; the provisioning system
must not leave old positives in place as a fallback after a known failure.

## Planning, execution and freshness

A production Change specifies service and actual artifact digest. Optional
`commit_sha` must match the signed source. The Context derives commit, CI and
dependency facts. The Plan pins source issuer, revision, hash, expiry and artifact
bindings, and all metadata/Policy hashes. Renewing or changing the source changes
the Plan hash, even when the new result is also PASS. It does not extend an old
approval. Use a new request ID for a new Plan; command replay preserves its
original scope and Plan instead of adopting new authority.

Both the Control Plane and customer Runner persist monotonic source fences.
Signed negative or expired latest revisions fence earlier positives. Source
change during signing or immediately before durable Grant admission is rejected.
Runner verifies the current source before obtaining deployment credentials and
again before Write. Grants cannot outlive source freshness. No provider read
credential, raw log or token enters the Grant/Policy Input/source audit.

Observation freshness is bounded by the configured ages. It is not a claim that
an unobserved provider change after capture is instantaneously known. Run and
provision the collector at the customer's required cadence; no failed collection
reuses a prior PASS. Missing/stale/untrusted files, keys, audit or DB prevent new
Write authorization. Read-only reconciliation and post-deploy observations remain
available after forward authority expires.

## Migration and validation

Quiesce production Writes. Upgrade the entire Control Plane/Worker/Runner fleet,
apply Control Plane migration 13 and Runner migration 007, provision independent
build/source public trust and fresh signed source data, then replan/reapprove.
Old observation audit DTOs and old Runs are not converted into authority. A
populated fence cannot be discarded by the ordinary down migration. A restored
or new database requires independently trusted minimum revision provisioning;
an old backup does not prove the latest source state.

Run:

```sh
go test -race -shuffle=on ./...
make test-signed-observation
```

These cover real adapter code with synthetic HTTP sources, wrong commit/artifact,
provider failure, schema/source mismatch, expiry, independent provenance tampering,
zero deployment credentials/Writes on invalid sources, DB restart fences and
changes immediately before durable admission. They do **not** satisfy the live
CI/observability staging acceptance in #13. Follow
[the external staging procedure](source-admission-staging.md) when ready.
