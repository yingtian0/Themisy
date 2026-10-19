# Signed Policy Bundle shadow comparison

`policy.CompareSignedBundles` compares the current and proposed signed Bundles
against the same canonical `policy.Input` cases. This is a read-only analysis
unit for Issue #10. It does not install a policy, change an Engine, re-plan a
release, issue a Grant, call an Adapter or authorize promotion/rollback.

The caller supplies an independently trusted issuer and key resolver, both
Bundles, one to 256 uniquely named cases and an explicit observation time. Both
Bundles are verified on every comparison: hash, signature, issuer, compatibility,
layer metadata, issuance and expiry must pass before compilation. A report from
an earlier valid comparison cannot be reused to skip those checks.

The JSON-serializable report identifies each Bundle hash/version and each input
hash. For every case it includes current/candidate Decision and Reason Codes,
added/removed Reason Codes and approval roles, and whether the Decision or
behavior changed. A version/hash change with identical decisions, reason sets
and role sets is a metadata-only update, not a changed case. Reason/role
collections are compared as sets; difference arrays are sorted. Raw inputs and
identity/delegation assertions are not included in the report.

The same input, including its existing pinned context, is used for both policies.
The comparator does not rewrite a pinned hash to make a candidate pass. Treat a
report as evidence about these exact synthetic or captured inputs, not proof of
all possible inputs or production safety. Inputs must be sanitized before use;
case IDs and reports may still identify operational policy choices.

## Failure and offline behavior

Empty/duplicate case IDs, unsupported input versions, too many/zero cases,
invalid Bundle signatures/lifetimes, compilation failures, invalid evaluation
results and cancellation return an error with **no report**. An error is not
"zero differences" and must not approve a candidate.

The analysis compiler removes nondeterministic builtins (network lookup/HTTP,
wall-clock time and randomness) and runtime introspection, and disables remote
schema references. Such candidates fail compilation rather than fetching data
or silently substituting a value. The existing enforcement compiler is unchanged.
This bounded analysis API does not add arbitrary network or command execution.

## Evidence and remaining work

Run `go test ./internal/policy` and `go test -race ./internal/policy`. Tests use
real signed synthetic Bundles and the actual embedded baseline. They cover
unchanged/deny comparisons, same canonical input hashes, reason/role differences,
caller-state preservation, invalid signatures/keys/issuer/expiry, malformed
Rego/evaluation, cancellation, case validation, no network calls and no reuse
after expiry. Existing hierarchy tests remain separate.

This first unit is an internal Go API; no service endpoint, automatic collector,
UI or CLI is connected yet. Registration/distribution/state APIs, durable audit,
Runner-applied hashes, staged rollout/rollback, maintainer review and live
operational verification remain open. The report is not a signed attestation or
a policy deployment record. Issue #10 stays open.
