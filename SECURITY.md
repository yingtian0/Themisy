# Security policy

## Project status

Themisy is a prototype and is not production ready. The in-memory store, mock adapter, and demo identity inputs do not provide a production security boundary. Do not connect this version to production credentials or production write APIs.

## Reporting a vulnerability

Do not open a public issue or pull request for a suspected vulnerability.

Use the repository's private security-advisory reporting feature (Security → Report a vulnerability; enabled 2026-11-05). Maintainer scope and absence handling: [MAINTAINERS.md](MAINTAINERS.md). Include:

- affected version or commit;
- component and deployment assumptions;
- reproduction steps or a proof of concept;
- expected security impact and possible attack path;
- any suggested mitigation;
- whether details have been shared elsewhere.

If private vulnerability reporting is unavailable, open a public issue containing no vulnerability details and ask the maintainers to enable or provide a private reporting channel. Do not send credentials, tenant data, or exploit details in that issue.

## Response process

Maintainers will acknowledge a report as soon as practical, normally within three business days. They will validate scope, establish a private remediation plan, coordinate a release and advisory, and credit the reporter unless anonymity is requested. Timelines depend on severity and release readiness; maintainers will provide status updates at least every seven days while a confirmed report remains unresolved.

Public disclosure is coordinated after a fix or documented mitigation is available. Please allow maintainers a reasonable remediation period before disclosure. This process is a project target, not a contractual service-level agreement.

## In scope

Reports are especially valuable when they concern:

- policy, approval, identity, or delegation bypass;
- Action Grant forgery, tampering, expiry bypass, or replay;
- cross-tenant or cross-runner authorization failures;
- credential exposure or access outside the customer Runner;
- idempotency failures causing duplicate external writes;
- audit tampering or writes proceeding without durable audit records;
- arbitrary command, HTTP, or cloud API execution through a typed interface;
- unsafe rollback, dependency-order, or missing-evidence behavior;
- supply-chain compromise of binaries, containers, policy bundles, or updates.

## Supported versions

There are no production-supported versions yet. Security fixes are applied to the default development branch until the project publishes its first versioned release and support matrix.

## Handling sensitive data

Never attach real credentials, tokens, customer identifiers, raw production logs, or personally identifiable information. Use the smallest synthetic reproduction possible. Maintainers must keep report access limited to people needed for triage and remediation.
