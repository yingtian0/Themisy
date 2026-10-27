# Offline exercise: edit one planned artifact value

Use this after the [offline planning course](offline-quickstart.md). It shows
how a desired version participates in the generated change. It does not inspect
or verify an image, prove that a version exists, create approval or execution
authority, issue a Grant, obtain credentials, or perform writes. The Context
and values remain synthetic.

## Make temporary copies

From the repository root, create a temporary directory and copy the checked-in
sample. The commands below leave the tracked file unchanged. They require Go
1.26.10 or later and `sed`; `jq` is optional for displaying selected fields.

```bash
tmpdir="$(mktemp -d)"
cp examples/offline/release.json "$tmpdir/release-original.json"
sed 's/"desired_version": "sha-demo"/"desired_version": "sha-demo-v2"/' \
  "$tmpdir/release-original.json" > "$tmpdir/release-edited.json"
```

Check the edited copy contains `sha-demo-v2` and the original still contains
`sha-demo`:

```bash
grep 'desired_version' "$tmpdir"/release-*.json
```

## Plan both inputs at the same time

Build one CLI binary and pass the same fixed time to both invocations. Options
come before the input filename.

```bash
go build -o "$tmpdir/themisyctl" ./cmd/themisyctl
"$tmpdir/themisyctl" offline-plan -at 2026-10-18T00:00:00Z \
  "$tmpdir/release-original.json" > "$tmpdir/plan-original.json"
"$tmpdir/themisyctl" offline-plan -at 2026-10-18T00:00:00Z \
  "$tmpdir/release-edited.json" > "$tmpdir/plan-edited.json"
```

Both commands should exit 0. Standard error prints `OFFLINE ONLY`; standard
output contains a Plan. If `jq` is installed, display the changed Step and its
hashes:

```bash
jq -r '.plan.phases[].steps[] | [.desired_version, .change_hash, .contract_hash, .profile_hash] | @tsv' \
  "$tmpdir/plan-original.json" "$tmpdir/plan-edited.json"
jq -r '.plan | [.hash, .plan_hash] | @tsv' \
  "$tmpdir/plan-original.json" "$tmpdir/plan-edited.json"
```

Otherwise inspect those fields in each JSON file. Confirm that:

- `desired_version` changes from `sha-demo` to `sha-demo-v2`;
- the Step's `change_hash` changes, and the Plan's `hash` / `plan_hash` changes;
- `contract_hash` and `profile_hash` stay the same because neither local
  Contract nor Profile changed; and
- both Plans still have the same fixed `created_at` and `expires_at` values.

The hashes are deterministic planner output, not signatures or external
evidence. A changed `desired_version` is merely a requested string; no registry
or artifact digest is checked. The Plan does not authorize execution. Remove
only the temporary directory you created when finished, for example with
`rm -r "$tmpdir"` after checking its value.
