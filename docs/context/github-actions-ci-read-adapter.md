# GitHub Actions CI read adapter

`adapters/githubactions.CIAdapter` is an allowlisted, read-only source unit. Its
configuration binds a Tenant, service, environment, repository, workflow, and
maximum age. A request supplies only the intended commit SHA and service target;
it cannot choose an owner, repository, workflow, or URL. The adapter issues GET
requests for runs filtered by `head_sha` and for artifacts belonging to the
selected run. It does not dispatch workflows, read logs, or download artifacts.

The selected run must identify the exact commit and configured workflow. The
newest run and attempt must be completed successfully and within the configured
maximum age. At least one nonexpired artifact with a valid SHA-256 archive
digest must belong to the same run and commit. Evidence records the Tenant,
repository, commit, workflow selector, run/attempt ID, source update time,
observed time, freshness, artifact IDs/digests, reason, and canonical hash.
GitHub's artifact digest is the digest of the uploaded artifact archive. It does
not establish an image digest or build provenance.

Missing runs/artifacts remain `MISSING`; unsuccessful runs are `FAIL`; partial,
stale, mismatched, unauthorized, rate-limited, or unavailable provider data is
`INCONCLUSIVE`. None of these states is converted to `PASS`.

This is an adapter contract and fixture-tested provider unit. A process-local
[CI snapshot collector](ci-snapshot.md) can retain its exact source observations
with tenant/target/capture-time/hash binding. Exported records are unsigned and
cannot be imported as trusted snapshots. The adapter is not wired
into Planner, Policy, Plan hashing, Grant issuance, or approval invalidation.
The existing `ci_success` request field therefore remains prototype input and
must not be used as production authorization evidence. Dependency health,
Contract/Profile provenance, end-to-end snapshot persistence, and real CI
staging validation remain open under Issue #13.

The fixture suite runs with:

```sh
go test ./adapters/githubactions ./pkg/credentials
```

The provider endpoints are GitHub's [workflow runs REST API](https://docs.github.com/en/rest/actions/workflow-runs)
and [workflow run artifacts REST API](https://docs.github.com/en/rest/actions/artifacts).
