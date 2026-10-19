# Local offline candidate packaging

This is the first preparation unit of #43, not a publishing workflow. From a
reviewed, committed checkout with Go 1.26.9+ and Python 3.12+:

```bash
python3 scripts/package-offline.py --output /tmp/themisy-offline-candidate
python3 scripts/verify-offline-package.py /tmp/themisy-offline-candidate
```

Choose a new output directory; existing files are never overwritten. The builder
archives HEAD into a temporary source directory, disables workspace/Go user
configuration for the build, and uses only committed source. Its default version
is `0.0.0-dev+<commit>`; `--version` accepts an explicit numeric candidate label.
A label is not a Git tag or maintainer-approved release. Do not package dirty
changes expecting them to be included: commit/review first.

Only native Darwin arm64 and Linux amd64 candidates are built. No execution
server/Runner image is provided by this offline-only unit. The archive contains
the CLI, examples, LICENSE, standalone guide, unsigned build manifest, structured
Go build metadata and a linked-module SPDX 2.3 SBOM (`sbom.spdx.json`). SHA256SUMS detects corruption when obtained from a
trusted source; it does not authenticate the builder or replace signed provenance.
`build-info.txt` remains an inventory; the separate SBOM describes the exact
binary, source commit and linked module versions/Go directory sums. It includes
compiler/standard-library version metadata, but does not decompose their files or
vendored components or the example assets, and makes no license-completeness,
vulnerability or authenticated-provenance claim. Unknown license conclusions are
`NOASSERTION`. Local/unversioned replacements or missing module sums fail the
builder rather than generating verified-looking metadata.

The smoke verifier checks the digest, embedded source/version, SBOM hash and
binary/source/module bindings, then runs examples from an extracted bundle outside
the checkout. It requires Go 1.26.9+ to reread the actual binary's BuildInfo with
workspace/user configuration and module fetching disabled. These verification
requirements do not change the standalone CLI's no-Go runtime course. It intentionally executes the bundled
binary, so use only your own trusted candidate. Normal/rejected commands need no
Go compiler or service while running. The regular CI builds/verifies a temporary
Linux candidate with read-only permissions; it does not upload, tag, release,
publish a container, or gain release permissions.

Remaining #43 gates: reviewed alpha version/tag/source; full component/file and
license review beyond the linked-module SBOM; authenticated provenance; an explicitly authorized release workflow with narrowly scoped
permissions; independent clean-machine installation; release notes/readiness and
upgrade review; #39 reporting channel readiness. Do not treat local candidates
as an approved alpha release. Ordinary PR CI permissions remain unchanged.

The document namespace is derived from the SBOM content; it is an identifier,
not a published download location. The creation timestamp is normalized to the
source commit's `source_date_epoch` for reproducibility and is explicitly not an
authenticated build time. SPDX structure and unknown-field conventions follow
[SPDX 2.3](https://spdx.github.io/spdx-spec/v2.3.1/package-information/).
Go `h1` directory sums are recorded as source metadata, not mislabelled as archive
SHA-256 checksums. The binary checksum is calculated from the actual file.
