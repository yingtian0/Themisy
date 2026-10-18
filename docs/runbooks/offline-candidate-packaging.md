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
the CLI, examples, LICENSE, standalone guide, unsigned build manifest and Go
dependency/build inventory. SHA256SUMS detects corruption when obtained from a
trusted source; it does not authenticate the builder or replace signed provenance.
The inventory is not a complete SBOM.

The smoke verifier checks the digest, embedded source/version and examples from
an extracted bundle outside the checkout. It intentionally executes the bundled
binary, so use only your own trusted candidate. Normal/rejected commands need no
Go compiler or service while running. The regular CI builds/verifies a temporary
Linux candidate with read-only permissions; it does not upload, tag, release,
publish a container, or gain release permissions.

Remaining #43 gates: reviewed alpha version/tag/source; full SBOM and authenticated
provenance; an explicitly authorized release workflow with narrowly scoped
permissions; independent clean-machine installation; release notes/readiness and
upgrade review; #39 reporting channel readiness. Do not treat local candidates
as an approved alpha release. Ordinary PR CI permissions remain unchanged.
