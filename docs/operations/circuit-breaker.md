# Production circuit breaker

The scheduler records terminal step outcomes in a rolling per-tenant production
window. When the configured minimum sample count is met and the error rate is
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

Until a durable distributed coordinator is configured, restarting the sole
worker clears its in-memory sample window. Treat worker restart while a circuit
is open as a safety-sensitive operator action and retain the audit/incident
record. Multi-worker production deployment requires a shared durable breaker
backend.

There is no durable breaker backend selectable in the current composition root.
The wording above is a release gate, not an available configuration switch.
Sharing a Temporal task queue or restarting another Worker cannot preserve its
history. Use the [restart procedure](worker-restart.md); leave affected groups
frozen if an operator cannot establish the prior safety state. This constraint
is tracked in [#50](https://github.com/yingtian0/Themisy/issues/50).

## Verification

The scenario suite asserts that opening the breaker produces zero new
production dispatches and that no public MCP tool can reset it:

```bash
go test -count=1 ./test/scenario -run CircuitBreaker
```
