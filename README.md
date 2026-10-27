# Themisy

Offline planning course: [English](docs/guides/offline-quickstart.md) · [日本語](docs/guides/offline-quickstart.ja.md).

**Non-production prototype / alpha preparation. Do not connect production credentials or production write APIs.**

Themisy connects a requested change to its approval, a narrowly scoped signed
Action Grant, customer-side execution, and the resulting evidence. The first
users are Platform/SRE developers using ECS and existing CI/CD; the first
execution target is one stateless service in dedicated staging.

The Control Plane coordinates planning and durable workflows. A customer-hosted
Runner holds credentials, independently authorizes the final operation, and
invokes a typed adapter against locally configured targets. This adds an
operation-specific approval and evidence boundary to existing CI/CD and IAM;
its adoption benefit over those tools has not yet been measured.

## Choose an entry point

| Course | Prerequisites and processes | External writes | Status |
|---|---|---|---|
| [15-minute offline planning](docs/guides/offline-quickstart.md) | Go 1.26.10+, local Contract/Profile/Intent files; CLI only | None; no credential acquisition, approval, or Grant | Normal/rejected examples locally verified; external timing not measured |
| Local execution development | PostgreSQL, Temporal, Control Plane, Worker, configured Runner; provisioned signing/trust material and test identity/context | Can dispatch writes if a real adapter is enabled | Developer assembly only; no turnkey safe mock runtime |
| [Dedicated ECS staging](docs/runbooks/ecs-runner-iam.md) | Above plus isolated AWS resources, fixed target mappings, STS roles, KMS/trust configuration, runtime/health sources | Real ECS updates and rollback | Not a validated onboarding course; live gate pending #6/#7/#11 |

Start with the offline course. [Readiness matrix](docs/readiness.md) records the
commit, verification types, evidence, known limits, and separate production gates.
No versioned alpha binary/image has been published by this work (#43).

## What is implemented

- Versioned Service Contracts and Release Profiles, strict validation, typed
  dependencies, deterministic release phases, canonical hashes and Plan expiry.
- PostgreSQL records/audit/outbox and Temporal workflows for approval, control
  signals, execution and compensation. The shared workflow coordinator persists
  service reservations, concurrency budgets and circuit state across workers;
  use the [coordinator acceptance and migration guide](docs/runbooks/workflow-coordinator.md).
- Signed Action Grant dispatch over outbound Runner long-poll, acknowledgements,
  authenticated reporting, freeze/drain, and signed results.
- Customer-side shared PostgreSQL journal, nonce/idempotency reservation, and
  read-only reconciliation of ambiguous external writes.
- Typed ECS and GitHub Actions adapters with Runner-local target mappings and
  credential brokers. These paths can perform external writes.
- ECS runtime checks and Datadog four-value evidence foundations. The standard
  ECS verification path now uses [source-bound read-only Runner observations](docs/verification/runner-observation-flow.md),
  with signed runtime/health results. Missing configuration or observations
  refuse success; this has not been validated against live AWS/Datadog.
- Opt-in, audited CI/dependency/metadata and runtime snapshots. These source
  observations are not yet the default production authorization Context.
- REST, CLI, structured MCP, and a minimal status/approval UI using the same
  application boundary. [Approval cards](docs/guides/approval-review.md) display the pinned change, declared
  impact, observation availability and explicit unknowns; they do not infer
  current artifacts or rollback authority.

Implementation and mock SDK tests do not establish a working production system.
The ECS runtime gate added in PR #37 and production verification guard in PR #36
are completed implementation units; parent #9 still tracks Evidence and workflow
integration. See the matrix for recovery, expiry and concurrency limits.

## Trust assumptions

Ordinary CI/Agent identities must have no parallel direct Write permissions for
this boundary to control writes. The Runner is trusted and remains inside the
trusted computing base. Its credentials stay in the customer environment; a
Grant cannot choose arbitrary shell, HTTP, AWS operations, roles, or provider IDs.

Local example identity, roles, scopes, CI and health assertions are synthetic;
request fields do not prove production identity or evidence. Production needs
verified identity/delegation, organization roles, trusted fresh Context, signed
Policy Bundles, and independent runtime observation. Missing, inconclusive, or
unknown results must not be treated as success. An expired lease or a lost
response does not authorize another write.

Review [architecture](docs/architecture/overview.md), [ADRs](docs/adr/README.md),
and the [threat model](docs/security/threat-model.md) before extending execution.

## Developer execution setup: prerequisites first

The example configurations are templates, not a ready-to-run deployment.
`config/themisy.example.yaml` references files **not included** in the repository:

- `/run/secrets/themisy-grant-ed25519`: provisioned mode-0600 development signing
  seed; production uses deployment-owned KMS signing.
- `/run/secrets/themisy-runner-token`: development Runner authentication.
- `/run/config/result-trust.json`: trusted result-signing keys.

`config/runner.example.yaml` additionally requires matching Grant trust, OIDC
trust/delegation material, signed Policy Bundle, manifest/result signing and
adapter-specific secret files and allowlists. Inspect its full configuration;
placeholders are not credentials. Production requires workload identity/mTLS and
shared durable journal state. The example Runner groups/tenant/targets must be
aligned with the chosen Contract and authenticated identity; they are not a
complete sample deployment. See [recovery and manifests](docs/runbooks/runner-recovery-and-manifests.md).

The Compose template defines PostgreSQL, Temporal, Control Plane, Worker and
Runner. It mounts the source tree but does not provision the above files. Its
startup is unverified as a complete course; do not assume it starts successfully
or executes a safe mock. Do not run integration teardown/migration tests against
an existing database. Use a dedicated disposable environment.

For source contributions, the non-service checks are:

```bash
make fmt-check
make test
make test-race
make lint
```

These four targets were rerun on the [recorded source commit](docs/guides/readme-verification.md).
That record also audits every README link, configuration prerequisite and
interface reference, and lists commands not executed against live services.
Service integration and live AWS checks are separately classified in the matrix.

## Interfaces

The existing `themisyctl plan` and `validate-contract` commands call a running
Control Plane over HTTP. `offline-plan` is explicitly separate and never calls
that server. Execution commands such as `start` can enqueue real signed Grants;
they are not part of the offline course.

REST routes are defined in [OpenAPI](api/openapi/v1alpha1.yaml). The stateless MCP
endpoint is `/mcp`; the minimal UI is `/ui/approvals`. Development actor/tenant/role
headers form a trusted-proxy boundary and must be stripped/replaced by verified
identity at an internet-facing ingress. They are not production authentication.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md), [governance](GOVERNANCE.md), and the
[Code of Conduct](CODE_OF_CONDUCT.md). Follow [SECURITY.md](SECURITY.md) for private
reports; never put vulnerability details in public Issues or PRs. Reporting
channel availability remains tracked in #39.

[Roadmap #52](https://github.com/yingtian0/Themisy/issues/52) is the current task
order. Licensed under [Apache License 2.0](LICENSE).
