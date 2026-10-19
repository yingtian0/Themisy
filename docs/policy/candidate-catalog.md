# Local signed Policy candidate catalog

`policy.OpenCandidateCatalog(directory, issuer, keys)` opens an existing,
operator-controlled local directory. `Register(ctx, bundle)` returns the exact
`sha256:...` hash after verification and offline compilation and writes the full
signed Bundle. `Get(ctx, hash)` returns a freshly verified copy. Close the
catalog when finished. Files use mode 0600; provision the directory with mode
0700. Linux/macOS local filesystems with hard-link and directory-sync support
are the initial scope.

Supply issuer and trusted keys separately from the Bundle. The clock is current
time; a historical Shadow comparison does not renew an expired Bundle. Read
operations recheck trust, expiry and compilation. A missing, invalid, expired,
oversized or corrupt object returns an error, never a successful empty object.
Repeated registration is safe for the same valid hash and cannot overwrite an
existing bad object. A directory-sync failure can return an error after an
object became visible; retry only the same registration, with full verification.

The directory is not a public upload location. It must remain operator-owned.
Automatic cleanup or repair is intentionally absent; an abrupt process death may
leave a `.candidate-*` temporary file. Inspect it separately, without treating it
as a registered candidate. Only exact hash filenames are retrievable.

This opt-in cache is disconnected from running applications. It has no active
policy, tenant assignment, rollout, Runner acknowledgement, HTTP endpoint or
durable deployment audit. Registration does not validate every possible policy
decision, prove mandatory baseline equivalence, authorize a Write, or count as
Issue #10's registration/distribution/status service API. Review
[ADR-0010](../adr/0010-local-policy-candidate-catalog.md) and use the
[Shadow comparison](shadow-comparison.md) for separate behavioral analysis.

Validation: `go test -race ./internal/policy`. Tests use signed synthetic Bundles
and real temporary files, including reopening a catalog and concurrent catalog
instances. No live Runner adoption or deployment is claimed.
