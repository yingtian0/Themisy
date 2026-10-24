# Read-only runtime observation collection

`ecs.Adapter.ReadRuntime` accepts the existing logical ReconcileRequest selector.
It reads the configured definition and ECS service/tasks using the same checks as
the deployment gate, but never calls UpdateService or retries a write. A confirmed
observation includes actual counts and rollout state. Any unconfirmed runtime
or definition-read result is INCONCLUSIVE with an empty summary; cancellation
and invalid selectors return no observation. Provider errors/ARNs are not exported.

`verification.NewRuntimeCollector` composes a trusted adapter, tenant, credential
broker, durable audit sink and a maximum age between one second and five minutes.
Configure an AWS verify-purpose role with only the required describe/list actions.
There is no fallback to a deploy role. The logical selector is checked before
credential acquisition. Run/Grant IDs provide caller-supplied correlation only;
they do not authenticate a requester or permit an action.

Collection writes a separate `runtime.observe` / `runtime_snapshot` audit record
containing schema, tenant/run/grant labels, observation time, maximum age, status,
summary and source/configuration hashes. Existing metric Evidence records remain
separate. Audit PASS describes the source observation, not a successful release.
Only `RequireFreshRuntime(now)` checks a returned snapshot's point-of-use freshness;
zero, imported, unconfirmed and expired snapshots fail. Records are independent
values and cannot restore trust through JSON. Recollect instead of using an audit
record as a positive cache.

Append failure/response loss, cancellation, clock reversal or expiry returns no
snapshot and does not retry the append. A response loss or expiry after append
may leave an inspection event without returning a usable snapshot. No existing
Runner, Result signature, Workflow, server or CLI defaults are changed. This
unit does not complete signed runtime attestation or runtime/Evidence composition.

Run `go test -race ./adapters/ecs ./internal/verification` and
`make test-runtime-observation-audit`. The latter creates a fresh dedicated local
PostgreSQL fixture, verifies confirmed/unconfirmed records across reopen and a
server-refused INSERT, then removes only its own resources. AWS SDK and credentials
are synthetic fixtures; live AWS acceptance is still unverified. See
[proposed ADR-0022](../adr/0022-separate-runtime-observation-audit.md).
