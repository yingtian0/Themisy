# Contributing to Themisy

Thank you for helping build a safer change-execution boundary. This project welcomes bug reports, design discussion, documentation, tests, adapters, and code contributions.

## Before opening an issue

- Search existing issues and accepted [architecture decisions](docs/adr/README.md).
- Do not disclose a suspected vulnerability in a public issue; follow [SECURITY.md](SECURITY.md).
- For a feature, describe the user problem, trust-boundary impact, failure behavior, and how it can be tested.
- For a bug, include a minimal reproduction, expected and actual behavior, and non-sensitive logs.

## Development setup

Go 1.26.9 or later is required.

```bash
make fmt-check
make test
make test-race
make lint
# Requires Docker for PostgreSQL and Temporal.
make test-integration
```

Use `make fmt` to format Go files. Pull-request CI also verifies that module files are tidy,
builds every package, enforces the current coverage floor, randomizes test order, repeats the
planner safety scenarios, lints workflow files, and scans reachable dependencies with
`govulncheck`.

Daily work follows the short [Issue-centered workflow](docs/agent-operations/README.md).
Run relevant local checks; batch complete CI after a coherent change.

## Change process

1. Keep a change focused on one behavior or one bounded Task Packet.
2. Add failure-path tests with, or before, a behavior change.
3. Preserve public API and persistence compatibility unless an accepted migration plan explicitly changes them.
4. Update user-facing documentation and relevant ADRs.
5. Explain the safety impact, tests, and remaining risks in the pull request.

Trust Boundaries, public protocol semantics, persistence authority, or cryptographic choices require an ADR. Start by proposing the decision and its alternatives; do not hide a boundary change inside a large implementation pull request.

## Safety-critical contribution rules

- Do not make an LLM a policy engine, credential holder, or workflow source of truth.
- Do not add arbitrary shell, arbitrary HTTP, or generic cloud API execution.
- Do not accept requester-supplied roles, scopes, health, or approval facts as trusted evidence.
- Do not turn missing or inconclusive evidence into success.
- Do not retry an unknown external write blindly.
- Do not add bypasses that default to allow while a future component is unavailable.
- Do not put real credentials, tenant data, secrets, or personally identifiable data in fixtures.

Changes to grant validation, policy enforcement, identity, credentials, idempotency, audit persistence, rollback, or execution ordering require explicit negative tests and review by at least two maintainers when the project has enough active maintainers.

## Commit certification

The project uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Sign off every commit:

```bash
git commit -s
```

The sign-off certifies that you have the right to submit the contribution under the project's Apache-2.0 license. The project does not require a separate Contributor License Agreement at this time.

## Review and acceptance

Maintainers evaluate correctness, security, compatibility, tests, operational impact, documentation, and consistency with accepted ADRs. A maintainer may ask for a smaller change, an ADR, additional evidence, or external security review. Acceptance follows the process in [GOVERNANCE.md](GOVERNANCE.md); submission does not guarantee inclusion.

All participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
