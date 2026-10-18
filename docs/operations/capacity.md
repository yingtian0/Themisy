# Capacity and release waves

Packet 05 schedules release steps against the intersection of all configured
limits. A step is dispatchable only when capacity is available for its tenant,
environment, region, cluster, owning team, risk tier, every shared failure
domain, and the exclusive service/environment lease.

The default development limits are 20 concurrent steps per tenant and
dimension, and one step per shared failure domain. These defaults are safe
demonstration values, not a production capacity recommendation. Contract data
supplies team, risk tier, runner group, and shared failure domains. Authenticated
request context supplies tenant, region, and cluster. Interface callers cannot
raise a limit in a release request.

## Wave construction

The planner fixes dependency order and scheduling context into the plan hash.
The scheduler then builds deterministic waves in phase and service-name order.
A dependency must complete in an earlier wave. Reservations are held for the
whole dispatch and released only on completion. A failed or unknown step stops
its downstream steps in the durable workflow.

## Backpressure

The effective dispatch allowance is the minimum of:

- Runner capacity currently available
- deploy Adapter rate-limit remainder
- remaining queue space
- every concurrency budget above

Zero or missing capacity fails closed before an execution reservation or
Adapter call. Runner freeze sets that Runner projection to zero capacity for
the authenticated tenant. The local demonstration seeds configured Runner
groups with capacity 20 so the mock workflow can run; production capacity must
instead come from authenticated Runner heartbeats.

## Capacity validation

`test/fixtures/scenarios/200-services.yaml` is a compact generator input. The
scenario expands it to 200 services and executes planning/dispatch simulation
ten times while checking dependency order, tenant limits, shared failure-domain
exclusivity, and a five-second eligible-to-dispatch p95 target.

Run:

```bash
make test-scenario
make benchmark
```

The in-process coordinator is shared by workflows handled by one worker
process. A single Temporal task queue can deliver Activities to different
Workers; it does not make their in-memory reservations or breaker state shared.
Multiple Worker processes and automatic failover/restarts do not provide global
exclusion or budget guarantees with this coordinator. Shared durable reservations,
fencing and breaker state remain [#50](https://github.com/yingtian0/Themisy/issues/50).

Even one Worker loses these maps when it restarts. Keep this development setup
to a single Worker, avoid overlapping old/new Workers during upgrades, and use
the [restart and unresolved-operation procedure](worker-restart.md). A lost
reservation does not prove that a provider Write did not start. Runner Journal
idempotency protects one operation; it does not exclude different Runs that
target the same service.
