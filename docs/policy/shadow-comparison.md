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

The internal Go API is also available through the offline `themisyctl policy-shadow`
command below. The opt-in [local candidate catalog](candidate-catalog.md) can
retain signed artifacts by exact hash with fresh validation on every read; it
does not apply them or preserve a deployment approval. No service endpoint,
automatic collector or UI is connected yet. Registration/distribution/state service APIs, durable audit,
Runner-applied hashes, staged rollout/rollback, maintainer review and live
operational verification remain open. The report is not a signed attestation or
a policy deployment record. Issue #10 stays open.

## Offline CLI example

Build the CLI, then compare the committed synthetic signed examples:

```bash
go build -o /tmp/themisyctl ./cmd/themisyctl
/tmp/themisyctl policy-shadow \
  -current examples/policy-shadow/current.json \
  -candidate examples/policy-shadow/candidate.json \
  -cases examples/policy-shadow/cases.json \
  -issuer example://offline-policy-shadow \
  -key-id synthetic-shadow-key \
  -public-key examples/policy-shadow/public.key \
  -at 2026-10-19T00:00:00Z
```

This example returns mode `OFFLINE_SHADOW_ONLY`, three cases and one changed
case: `frozen-service` changes from ALLOW to DENY with `SERVICE_FREEZE`. The
missing-subject case stays DENY. The fixtures were signed with a discarded
synthetic development key; only its public key is committed. Never configure
that sample key as production policy trust.

The frozen `-at` is the analysis time for this historical fixture. Omit `-at`
for current-time analysis of real operator-supplied Bundles. Backdating an
analysis does not renew a Bundle or authorize its deployment. The example's
expired Bundles intentionally fail at the current time; do not extend or ignore
expiry to make them eligible for execution.

Supply issuer/key ID/public key independently of candidate content. This CLI
unit supports one explicitly trusted Ed25519 key for both Bundles; the internal
API supports a separately configured resolver for multi-key comparisons. Files
must contain one bounded JSON value with known fields; cases use the canonical
Input schema. No server address, cloud credential or actor role is used by this
command. Building the binary can still require Go module downloads.

A successful comparison exits 0 **whether or not cases differ**. Inspect the
report's Decisions, Reason Codes and `changed_cases`; exit 0 means only that
analysis completed. Invalid input/signature/key/expiry, restricted builtins,
evaluation failure or cancellation exits 1 with no JSON report. `policy-shadow
-h` exits 0 and explains the same limits. Preserve Bundle/input hashes with any
review record; the CLI does not write a durable policy deployment audit.
