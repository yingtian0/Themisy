# Read-only dependency health adapter

Issue #13 now has an opt-in `datadog.NewHealthReader` source adapter. It reads
metrics for **one configured dependency target**, without a deployment execution
ID. It does not implement the complete dependency Context or authorize actions.
See [ADR-0017](../adr/0017-dependency-health-observations.md) (Proposed).

The operator supplies `HealthConfig.Targets`, keyed by tenant, dependency service,
and environment. Each `HealthTarget` selects a supported Datadog site and trusted
`Query` (expression, comparator, threshold, aggregation, minimum points, maximum
age) plus a whole-second observation window between one minute and 24 hours.
The constructor copies the catalog. The caller cannot override these selectors
through `HealthRequest`; unconfigured tenants/services/environments fail before
HTTP. This logical mapping is not user authentication or a Directory check.

Trusted customer-side composition supplies a `credentials.Credential` containing
read-scoped `api_key`/`app_key` values and expiry. It must keep these outside agent
and API input. This unit does not add production composition or a credential
source. The default client has a 15-second timeout and refuses redirects;
injected `HTTPDoer` implementations are trusted dependencies, not request fields.

One GET uses `/api/v1/query`; query bounds are Unix seconds and response bounds
and samples are Unix milliseconds. The response must echo the configured query
and exact bounds. See the [Datadog endpoint reference](https://docs.datadoghq.com/api/latest/metrics/query-timeseries-points/).

| Result | Meaning |
|---|---|
| PASS | Every returned series is complete, fresh, and meets the configured threshold |
| FAIL | Complete/fresh observations include a breached series threshold |
| MISSING | A valid, matching response contains no series |
| INCONCLUSIVE | Partial/invalid/stale data or provider/auth/rate-limit failure prevents a conclusion |

A malformed peer overrides a positive or negative threshold conclusion with
INCONCLUSIVE; it is not dropped. Each series needs its own minimum count of
unique, finite samples inside the window. The latest sample must remain strictly
inside its maximum-age deadline at capture time. `SourceUpdatedAt` retains the
oldest of the per-series latest timestamps. `last` selects by timestamp, not
response order. Every series is evaluated separately; a healthy peer cannot
conceal a threshold breach.

`QueryHash` binds the tenant/target and complete operator query configuration.
`SamplesHash` binds sorted timestamp/value samples for all validated series;
reordering data does not change it, but changing a sample does. It is absent if
the sample set could not be validated. `EvidenceHash` binds the entire returned
record except itself. A single-series result includes its aggregate value;
multi-series results do not invent one combined measurement. Keys, raw query,
provider body and raw errors are not included in the output.

These are inspection hashes, not signatures. `HealthEvidence` is an exportable,
freely constructible DTO, not authenticated restored state. Do not accept JSON
from a requester as a source observation or infer deployment revision, image
provenance or execution authority from these metrics. A trusted dependency-set
collector, broker composition, immutable Context snapshot, durable audit,
Plan/Grant hash pinning and invalidation are still required. There is no new
HTTP route or enabled production path in this change.

Run fixture verification with:

```bash
go test -race -shuffle=on ./adapters/datadog
```

The tests use synthetic data only. Live Datadog staging and complete dependency
Context acceptance remain unverified.
