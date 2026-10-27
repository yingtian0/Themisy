# ADR 0024: Source-bound read-only Runner verification

- Status: Proposed
- Date: 2026-10-27
- Scope: #9; observation after expiry also supports the read-only part of #51

## Problem

The standard GrantExecutor dispatched writes but had no verification executor.
Runtime and metric readers existed separately. Reusing a deploy/rollback Grant
would either permit a second Write or stop observation when forward authority
expired. API acceptance cannot prove real placement or health.

## Decision

Use the existing signed outbound delivery/report transport for a distinct
`release.verify` capability. Its signed selectors identify the completed source
Grant, journal nonce, canonical request hash, idempotency key, source operation,
external execution and exact observation window. This capability is never
passed to the write dispatcher, policy's Write path, or a deploy credential.
The Runner requires a durable SUCCEEDED source record in the same tenant/group,
Run, step and target. UNKNOWN/reserved/unbound sources are refused before reads.

The Control Plane derives requests from durable successful dispatch records and
the pinned Plan's observation duration. It does not accept caller source IDs,
URLs, provider resource IDs, query expressions or boolean health assertions as
observation authority. Grant issuance for writes remains unchanged. Read expiry
is fresh and bounded; an expired source Plan/identity cannot enable a Write but
does not prevent authenticating an existing operation's truth.

Read ECS through a separate purpose-scoped Role and local target mappings, and
read metrics through configured Datadog queries/local credentials. Preserve the
runtime body (counts, definition/container hashes, confirmed container digests),
raw metric Evidence and a combined Evidence decision separately. The combined
hash binds both bodies. FAIL takes precedence, then MISSING, INCONCLUSIVE, PASS.
Only matching PASS runtime and PASS metrics can produce PASS. Never invent
runtime counts for an unconfirmed observation.

The existing domain-separated result-report/v2 signature covers all bodies,
source request hash, Runner identity, correlation IDs and external execution.
Result ingestion validates source/window/runtime/combined hashes before its
atomic result/audit commit. The audit retains the complete signed report/key ID.
Workflow verification consumes that durable result and projects source/receipt
IDs and runtime facts to the tenant-scoped API and UI. It never assigns success
from a portal or ECS UpdateService acknowledgement.

Metric sample time is distinct from collection time. `sampled_at` retains the
latest actual datapoint while `observed_at` records collection. Historical
samples remain subject to the configured freshness bound and requested window.
The signed query is chosen only from Runner configuration; credentials are not
part of Evidence.

## Compatibility and migration

New optional selectors/observation fields are omitted for old write Grants and
reports; old canonical bytes remain supported. Old clients cannot execute the
new capability. Upgrade all Control Plane/Workers/report consumers and customer
Runners together before enabling it. Drain/fence old writers, preserve existing
journals/manifests, and reconcile in-flight/UNKNOWN operations read-only.

ECS startup now requires `release.verify`, a distinct `verify_role_arn`, a trusted
health query for every configured ECS target, and valid local health credentials.
Production requires the ECS verification path; unsupported runtime providers are
refused. The development GitHub adapter remains a separate development path,
not a complete verified production release. Provider IAM must actually restrict
the verify Role to reads (#7/#11 live acceptance remains separate).

New Deployment results retain the originating Grant ID, including recovered
results. Older results without that binding stop verification explicitly; do
not guess the source or replay the write to manufacture a new result. Store
schema ownership and the Workflow command graph are unchanged. The existing
4-KiB signing bound remains in force; oversized reports fail closed.

## Consequences and remaining gates

A crash can repeat an authorized read, but never dispatch a Write on this path.
A durable completed observation is replayed without another read. Lost report
submission can be retried against the same observation. Audit/signature/provider
failures never become PASS. A delivery that expires before observation completes
remains unavailable; operator re-observation is not permission to repeat a Write.

This decision does not enable the future compensation epoch/expiry design of
ADR-0011 or complete #10/#12/#13. Source freshness and identity must still be
connected to new Write authorization. Live AWS/IdP/observability staging evidence
is not implied by synthetic Provider or real PostgreSQL tests. Proposed status
is not an independent maintainer approval or production support declaration.
