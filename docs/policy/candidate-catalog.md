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
[ADR-0013](../adr/0013-local-policy-candidate-catalog.md) and use the
[Shadow comparison](shadow-comparison.md) for separate behavioral analysis.

Validation: `go test -race ./internal/policy`. Tests use signed synthetic Bundles
and real temporary files, including reopening a catalog and concurrent catalog
instances. No live Runner adoption or deployment is claimed.

## CLI

Build `go build -o /tmp/themisyctl ./cmd/themisyctl`, provision an empty local
directory, then use a currently valid operator-signed Bundle and independently
configured public key:

```bash
mkdir -m 700 ./policy-candidates
/tmp/themisyctl policy-candidate register \
  -directory ./policy-candidates -bundle ./signed-candidate.json \
  -issuer YOUR_TRUSTED_ISSUER -key-id YOUR_TRUSTED_KEY_ID \
  -public-key ./trusted-policy-public.key
/tmp/themisyctl policy-candidate get \
  -directory ./policy-candidates -hash sha256:EXACT_HASH_FROM_REGISTRATION \
  -issuer YOUR_TRUSTED_ISSUER -key-id YOUR_TRUSTED_KEY_ID \
  -public-key ./trusted-policy-public.key
```

The placeholder paths/issuer/key/hash must be supplied by the operator. Do not
use the committed historical Shadow fixture as a currently valid candidate or
configure its sample key as production trust. There is no `-at` clock override:
an expired Bundle must be reissued through the actual signing/review process.
Both commands emit `mode: LOCAL_CANDIDATE_ONLY` and `hash`; `get` also emits the
complete signed `bundle`. This is an artifact result, not rollout status or a
Runner acknowledgement. Successful operations exit 0; validation/storage errors
exit 1 with no success JSON. An output-write failure can occur after successful
storage, so use the same hash for a verified retry. `-h` prints help and exits 0.

Neither command contacts the server, even if `-server` or actor environment
variables are set. Registration writes only this explicitly chosen local cache;
retrieval does not update the cache. The commands do not create missing
directories, fetch candidates, sign them, repair corrupted objects or activate
policies. CLI tests sign fresh synthetic Bundles in temporary directories, verify
register/get/idempotency and HTTP request count zero, and reject stale/tampered
inputs, wrong trust, path-like hashes and clock overrides without success JSON.
