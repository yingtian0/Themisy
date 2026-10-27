# README verification: 2026-10-27

Baseline: main [`488045e217177fe630f2a7324d03f40a2e5aaa24`](https://github.com/yingtian0/Themisy/tree/488045e217177fe630f2a7324d03f40a2e5aaa24).
Host: macOS arm64, Go 1.26.10. This records developer checks, not an independent
onboarding trial or production certification.

## Command and interface inventory

| README entry | Check / result |
|---|---|
| `make fmt-check` | Executed locally; PASS |
| `make test` | Executed locally; PASS |
| `make test-race` | Executed locally; PASS |
| `make lint` | Executed locally; PASS (`go vet ./...`) |
| `offline-plan` course | Built CLI normal/refused/cycle/help/artifact examples revalidated in the [offline record](offline-verification.md#revalidation-on-2026-10-27); no service/credential/Write path |
| `themisyctl plan`, `validate-contract` | Inspected routing in `cmd/themisyctl/main.go`; these use HTTP. Not invoked against a running Control Plane in this review |
| `start` | Inspected HTTP routing and application Grant composition; deliberately not invoked against a live service |
| `/mcp`, `/ui/approvals`, OpenAPI | Matched registered routes in `cmd/themisy/main.go`, `internal/ui/server.go` and checked-in API specification. UI handlers/forms tested with synthetic in-process fixtures; no deployed ingress claim |
| Compose template | Configuration rendered without starting containers. Complete service startup remains unverified; missing provisioned files and identity/target mismatch prevent treating it as a turnkey course |

All README relative links resolve to tracked repository files. The external
roadmap link identifies the existing #52 tracker. The offline English/Japanese
entry points and readiness matrix exist on this baseline. Source inspection
matched outbound Grant dispatch, ECS runtime preflight, shared Runner journal,
durable workflow coordinator and the unavailable default verification executor.
The separate opt-in observation collectors do not imply default production
Context wiring. Normal CI still needs direct provider Write denied, and Runner
trust remains an assumption.

## Configuration preflight inventory

`config/themisy.example.yaml` requires development Grant signing seed,
Runner token and result trust bundle at the absolute paths shown in README.
Those files are not checked in. KMS/mTLS alternatives need deployment-owned
configuration; no working cloud key or role is bundled.

`config/runner.example.yaml` requires these externally supplied paths/values:

| Variable | Required material |
|---|---|
| `THEMISY_RESULT_PRIVATE_KEY_FILE` | Runner development result signing seed |
| `THEMISY_RUNNER_TOKEN_FILE` | Matching development registration token |
| `THEMISY_GRANT_PUBLIC_KEY_FILE` | Matching Control Plane Grant verification key |
| `THEMISY_OIDC_PUBLIC_KEY_FILE` | Development OIDC verification key |
| `THEMISY_DELEGATIONS_FILE` | Provisioned delegation records |
| `THEMISY_RUNNER_DATABASE_URL` | Customer-side shared Runner journal PostgreSQL |
| `THEMISY_PLAN_MANIFEST_FILE` | Independently provisioned signed approval manifest |
| `THEMISY_MANIFEST_PUBLIC_KEY_FILE` | Manifest verification key |
| `THEMISY_GITHUB_TOKEN_FILE` | Runner-local token when enabling GitHub adapter |
| `THEMISY_DATADOG_CREDENTIAL_FILE` | Runner-local credentials when enabling Datadog adapter |

The example signed Policy Bundle is a development fixture, not a production
trust policy. Align tenant, authenticated identity, Runner group, logical
service/environment, artifact digest and local target allowlists. Compose does
not provision the signing/trust files, the HTTPS example Control Plane address,
OIDC/delegation/manifest authority or external repositories. Empty/default
adapter placeholders do not establish an executable sample.

Not performed: complete Compose startup, real AWS/STS/ECS/CloudTrail, external
identity/directory, production ingress, published alpha installation or an
independent user trial. Follow #6/#7/#11/#12/#13/#39/#43/#47 for those gates.
