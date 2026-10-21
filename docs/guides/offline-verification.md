# Offline planning verification record

Date: 2026-10-18 (Asia/Tokyo). Baseline: main
`8eb8a400d4df468036e101927c177cbd66a5d723`; implementation is the commit adding
this record (see Git history). Host: macOS arm64, Go 1.26.9.

| Check | Result |
|---|---|
| `go test ./cmd/themisyctl ./internal/planner ./internal/contract ./internal/profile` | PASS |
| `go test -race ./cmd/themisyctl` | PASS |
| `go vet ./cmd/themisyctl` | PASS |
| `go build -o /tmp/themisyctl-offline-41 ./cmd/themisyctl` | PASS |
| Built CLI normal example, fixed `-at 2026-10-18T00:00:00Z` | exit 0, plan hash `sha256:4131b191474aeb56ad25713a7d7e139037cb6dad2179668c03f9b85eedb7f9c9` |
| Built CLI rejected example | exit 1, `unknown service "missing-api"`, no plan |
| CLI vs core planner with fixed input/time/context | Full typed Plan equality, including timestamps and hashes |
| Offline HTTP guard | No HTTP calls with rejecting default transport and invalid server address |
| Invalid Contract graph / schema / extra fields / extra JSON | Explicit refusal, no plan |

Code inspection and the CLI dependency list show that the binary imports the
domain, Contract/Profile loaders and planner; it does not import execution,
Grant issuance, credential brokers or application services. No backend services
or cloud credentials were used in the CLI examples. Existing CLI HTTP tests use
an in-process test server; that is separate from the offline path.

Not performed: a fresh-machine dependency installation, external participant
15-minute measurement, real PostgreSQL/Temporal, real AWS, staging deployment,
release artifact verification. These checks are not implied by local success.
Use the observation table in [the guide](offline-quickstart.md) for independent
trials. A versioned binary/distribution is still tracked in Issue #43.
