# Isolated Workflow recovery acceptance

Run `make test-workflow-recovery` from the source checkout with Go and Docker.
The target creates its own PostgreSQL 17.6, Temporal 1.29.6.1, Docker network and
loopback-only dynamic ports. It ignores any caller-provided database address.
It does not use the normal Compose database, stop existing services, contact AWS
or publish artifacts. Allow roughly six minutes for the fixed server timeouts.

The target runs race-enabled integration tests for:

| Boundary | Evidence |
|---|---|
| Saved Deployment response loss | PostgreSQL reconnect restores the complete result |
| Existing Grant recovery | Same Execution and Grant survive a store replacement |
| UNKNOWN + cancel | Worker replacement preserves CANCELLED/UNKNOWN; server history replays |
| Signed deploy/rollback recovery | Real issuance, HTTP signature verification and durable coordinator; independent verification after recovery |
| Expiry/cancel race | Expired Plan/Grant do not permit another Write or overwrite cancellation |
| OS process crash | First Worker process is killed while a recovery completion is pending; replacement process reads the same Grant; no replacement Write |

All provider effects, initial identity/delegation facts and health observations
are synthetic. Runner Journal/provider reconciliation, final Runner policy
acceptance, actual AWS IAM and live runtime observations are separate gates.
Passing this target is not production readiness or completion of Issues #9/#11.

Only successfully created resource IDs are removed by the cleanup trap. The
script never removes pre-existing resources by name or runs Compose down. On
failure it keeps service diagnostics in a newly allocated temporary directory
and prints that location; inspect and delete that directory when no longer
needed. Test subprocess logs are in their Go test temporary directories. A
forced termination of the entire test harness can prevent cleanup: inspect the
specific `themisy-recovery-*` resources from that run before removing them. Do
not issue broad container/volume prune commands.

The existing read-only PR CI integration job invokes the same target. Its
addition does not change workflow triggers or GitHub permissions. A feature
branch push alone still does not run PR CI; record the exact commit/run when a
PR can be opened. Explicit-address tests invoked directly skip when either
required address is absent; that skip is never live acceptance evidence.

For older-history command compatibility, ordinary `go test ./internal/workflow`
replays the two committed pre-marker main histories. Their exact source and
regeneration instructions are in
[the fixture README](../../internal/workflow/testdata/README.md).
