# Production circuit breaker

The PostgreSQL coordinator records terminal and unknown step outcomes in a
rolling per-tenant production window. When the configured minimum sample count
is met and the error rate is
at or above the policy threshold, the circuit opens. Every subsequent
production step is rejected before execution reservation and Adapter dispatch.
Staging dispatch is unaffected so recovery can still be validated safely.

The development policy uses a 20-result window, at least five samples, and a
50% error-rate threshold. Production values must be owned by platform policy
and tuned from observed traffic and failure cost.

## Recovery

There is deliberately no `close`, `reset`, or policy-mutation operation in the
REST API, CLI, MCP tools, or AI-facing Application use case. An agent cannot
close the breaker. Operators must first identify the failing change, freeze
affected Runners when necessary, validate recovery in staging, and use an
operator-only recovery procedure provided by the deployment environment.

The circuit state and policy are stored with the shared PostgreSQL coordinator,
so worker restart does not clear the sample window or open state. Workers fail
closed if they cannot read or update that shared state. Configuration changes
require a coordinated migration/operations procedure; workers reject a policy
that differs from the one already stored for the tenant.

## Verification

The scenario suite asserts that opening the breaker produces zero new
production dispatches and that no public MCP tool can reset it:

```bash
go test -count=1 ./test/scenario -run CircuitBreaker
```
