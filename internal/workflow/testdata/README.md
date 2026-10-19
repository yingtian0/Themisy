# Pre-version-marker Workflow histories

These synthetic histories were captured from a dedicated Temporal 1.29.6.1
server running **main `8eb8a400d4df468036e101927c177cbd66a5d723`**, using Go SDK
from that revision. No recovery/scheduler version markers existed in that
Workflow. No cloud operation, real credential, customer identity or private
security report is included.

- `main-8eb8a40-deploy-unknown.json`: mock deploy failure; old Run BLOCKED,
  Step UNKNOWN.
- `main-8eb8a40-rollback-unknown.json`: mock verification failure and mock
  rollback failure; old Run/Step ESCALATED. The filename describes the ambiguous
  operation, not its old projected Step status.

`TestMainUnknownHistoriesReplay` runs the **current** Workflow against both
server histories, without running Activities or connecting to Temporal. It
checks command compatibility. Replay does not establish every old history's
compatibility or assert that all future Activity inputs have equivalent values.
DefaultVersion branch tests separately check current branching behavior.

The history events and Activity/Workflow payloads are retained. Only transport
`identity` and sticky queue names were normalized to synthetic names to avoid
committing a developer workstation name; no command, version marker or payload
was inserted/deleted to make replay pass.

## Regeneration

`export_legacy_history_test.go.txt` is the complete synthetic fixture exporter.
Use an isolated **clean checkout of the exact main revision above**, not the new
Workflow. Make a Go overlay that maps a new
`internal/workflow/legacy_history_export_test.go` in that checkout to the
exporter's absolute path. With a dedicated disposable Temporal server, run:

```bash
THEMISY_TEMPORAL_ADDRESS='DISPOSABLE_TEMPORAL:7233' \
THEMISY_LEGACY_HISTORY_DIRECTORY='/absolute/path/to/output' \
  go test -overlay /absolute/path/to/overlay.json ./internal/workflow \
    -run '^TestExportLegacyUnknownHistory$' -count=1
```

The output directory must exist. The exporter uses only an in-memory application
store and mock executor; it creates two synthetic workflows on the given server.
Normalize infrastructure identities as described above, inspect the JSON for
non-synthetic data, then replay on the new version. Never use a shared or
production Temporal namespace. Replacing fixtures from a new version would erase
the upgrade-compatibility regression and is not a valid refresh.
