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

PostgreSQL-backed scheduling reservations, capacity counters, write admissions,
and production circuit samples are shared by Control Plane workers. Each
reservation is bound to an owner token and monotonically increasing fencing
value. A worker must present the current lease before it can persist a write
admission; a stale owner is rejected. Expiry can reclaim only a reservation
that has not been admitted for an external write.

An admitted or ambiguous external write becomes `UNKNOWN` and continues to hold
all capacity dimensions across worker restart. It cannot be cleared by a
workflow retry or a public API. Operators must reconcile the external action
and record its terminal outcome through the controlled recovery procedure.
Loss of the shared PostgreSQL coordinator fails closed; workers do not fall
back to process-local budgets. Runner capacity, Adapter rate limits, and queue
pressure remain separate required admission signals and must also be available.

Before running multiple workers, apply the Control Plane migration to the
shared database, ensure every worker uses that database and the same scheduling
configuration, and verify an UNKNOWN reservation blocks a second Run after a
worker restart. This coordinator is separate from the customer Runner journal:
it controls Control Plane dispatch admission and does not grant Runner write
authority.

The memory-store development mode remains limited to one Worker and loses
reservations on restart. A Temporal queue does not share that memory. Use the
[restart procedure](worker-restart.md) for memory-mode inventory and reconciliation;
never treat lost reservations as permission to repeat an unknown Write.
