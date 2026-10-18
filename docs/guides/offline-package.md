# Offline CLI candidate

This is a local development candidate, not a published or production-supported
release. No execution image, signed provenance or SBOM is included. The source
commit and candidate version are recorded; the manifest is unsigned and cannot
authenticate who built the archive. Do not equate a checksum with trusted origin.

Use only a candidate from a source/builder you trust. Before extraction, verify
the archive against its accompanying `SHA256SUMS` with your platform's SHA-256
tool. For example, on macOS from the directory containing both files:

```bash
shasum -a 256 -c SHA256SUMS
```

On Linux, use `sha256sum -c SHA256SUMS`. A mismatch means stop; do not edit the
checksum to make it pass. Neither command verifies a signature or provenance.

Extract the archive into a new directory. From its bundle root:

```bash
./themisyctl version
./themisyctl offline-plan -at 2026-10-18T00:00:00Z examples/offline/release.json
./themisyctl offline-plan examples/offline/rejected.json
```

Expected exits are 0, 0, and 1. The rejected example reports
`unknown service "missing-api"`; it emits no Plan. Normal output labels the
Context synthetic and contains one staging identity-api step. These commands
need no Go installation, cloud credentials, Control Plane, DB, Temporal or Runner.
They create no approval/Grant, acquire no credentials and make no adapter Write.
Do not use execution commands as part of this candidate course.

Compare `version.source_commit` with `build-manifest.json` and the intended
reviewed source. `build-info.txt` is a Go build/dependency inventory, not a
complete SBOM or an authenticated attestation. Only the native platform stated
in the filename is provided; packaging currently accepts macOS arm64 and Linux
amd64. Cross-platform runtime support is not inferred from a successful build.

There is no background service or database migration to stop. To update, verify
a new candidate in another directory, record its source/version and repeat the
normal/rejected examples. Do not overwrite existing files blindly. Existing
server APIs, protocols and production Plans are unchanged by this CLI package.

Release blockers remain: private reporting availability (#39), reviewed alpha
tag/source, complete SBOM/signed provenance and independent clean-machine/user
verification (#43/#47). No release publication is authorized by this guide.
