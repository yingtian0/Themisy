"""Build a local, source-pinned offline CLI candidate. Never publish or tag."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


def run(*args, cwd=None, env=None):
    return subprocess.check_output(args, cwd=cwd, env=env)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path, help="new output directory")
    parser.add_argument("--version", help="candidate label; default is commit-scoped dev version")
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    commit = run("git", "rev-parse", "HEAD", cwd=repo).decode().strip()
    epoch = int(run("git", "show", "-s", "--format=%ct", commit, cwd=repo))
    version = args.version or "0.0.0-dev+" + commit[:12]
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", version):
        parser.error("version must be a numeric semantic-version candidate without path characters")
    host = run("go", "env", "GOHOSTOS", "GOHOSTARCH", cwd=repo).decode().split()
    if tuple(host) not in (("darwin", "arm64"), ("linux", "amd64")):
        parser.error("candidate packaging currently supports native darwin/arm64 or linux/amd64 only")
    output = args.output.resolve()
    if output.exists():
        parser.error("output directory must not exist; existing artifacts are never overwritten")
    with tempfile.TemporaryDirectory(prefix="themisy-offline-build-") as temp:
        root = Path(temp)
        source = root / "source"
        source.mkdir()
        # Only committed public source is used; working-tree edits and secrets
        # are never copied into the package or consumed as build inputs.
        archive = run("git", "archive", commit, cwd=repo)
        with tarfile.open(fileobj=io.BytesIO(archive)) as archive_file:
            archive_file.extractall(source, filter="data")
        bundle = root / f"themisyctl_{version}_{host[0]}_{host[1]}"
        bundle.mkdir()
        env = dict(os.environ, CGO_ENABLED="0", GOOS=host[0], GOARCH=host[1],
                   GOWORK="off", GOENV="off", GOFLAGS="-mod=readonly")
        run("go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
            f"-X main.version={version} -X main.sourceCommit={commit}",
            "-o", str(bundle / "themisyctl"), "./cmd/themisyctl", cwd=source, env=env)
        reported = json.loads(run(str(bundle / "themisyctl"), "version"))
        if reported != {"version": version, "source_commit": commit}:
            raise RuntimeError("candidate binary does not report the selected committed source; commit version support first")
        shutil.copytree(source / "examples", bundle / "examples")
        shutil.copyfile(source / "LICENSE", bundle / "LICENSE")
        shutil.copyfile(source / "docs/guides/offline-package.md", bundle / "README.md")
        info = run("go", "version", "-m", str(bundle / "themisyctl")).decode().splitlines()
        (bundle / "build-info.txt").write_text("\n".join(info[1:]) + "\n")
        (bundle / "build-manifest.json").write_text(json.dumps({
            "kind": "unsigned-local-build-manifest", "version": version,
            "source_commit": commit, "source_date_epoch": epoch,
            "go_version": run("go", "version").decode().strip(),
            "platform": "/".join(host), "scope": "offline planning only",
            "attestation": "none; not a signed provenance statement or SBOM",
        }, indent=2) + "\n")
        output.mkdir(parents=True)
        artifact = output / (bundle.name + ".tar.gz")
        with artifact.open("wb") as raw:
            with gzip.GzipFile(fileobj=raw, filename="", mode="wb", mtime=epoch) as compressed:
                with tarfile.open(fileobj=compressed, mode="w") as tf:
                    for path in sorted(bundle.rglob("*")):
                        if not path.is_file():
                            continue
                        metadata = tf.gettarinfo(str(path), str(path.relative_to(root)))
                        metadata.mtime, metadata.uid, metadata.gid = epoch, 0, 0
                        metadata.uname = metadata.gname = ""
                        metadata.mode = 0o755 if path.name == "themisyctl" else 0o644
                        with path.open("rb") as content:
                            tf.addfile(metadata, content)
        digest = hashlib.sha256(artifact.read_bytes()).hexdigest()
        (output / "SHA256SUMS").write_text(f"{digest}  {artifact.name}\n")
        print(f"LOCAL CANDIDATE ONLY: {artifact}")
        print(f"source_commit={commit}; no tag, release, image, SBOM or signed provenance published")


if __name__ == "__main__":
    main()
