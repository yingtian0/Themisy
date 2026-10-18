"""Smoke-test a trusted, locally built offline candidate; never authenticate origin."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile


def verify(directory):
    lines = (directory / "SHA256SUMS").read_text().splitlines()
    if len(lines) != 1:
        raise ValueError("expected exactly one native archive")
    expected, name = lines[0].split("  ", 1)
    if Path(name).name != name or not name.endswith(".tar.gz"):
        raise ValueError("invalid archive name")
    archive = directory / name
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
        raise ValueError("archive checksum mismatch")
    with tempfile.TemporaryDirectory(prefix="themisy-offline-verify-") as temp:
        with tarfile.open(archive) as tf:
            tf.extractall(temp, filter="data")
        roots = list(Path(temp).iterdir())
        if len(roots) != 1 or not roots[0].is_dir():
            raise ValueError("expected one bundle directory")
        root = roots[0]
        manifest = json.loads((root / "build-manifest.json").read_text())
        version = json.loads(subprocess.check_output([str(root / "themisyctl"), "version"], cwd=root))
        if version != {"version": manifest["version"], "source_commit": manifest["source_commit"]}:
            raise ValueError("version/commit mismatch")
        for fixture, code in (("release.json", 0), ("rejected.json", 1)):
            result = subprocess.run([str(root / "themisyctl"), "offline-plan", "-at",
                "2026-10-18T00:00:00Z", "examples/offline/" + fixture], cwd=root,
                capture_output=True, text=True, check=False)
            if result.returncode != code or "OFFLINE ONLY" not in result.stderr:
                raise ValueError(f"unexpected {fixture} result: {result.returncode}")
            if code == 0:
                output = json.loads(result.stdout)
                if output["mode"] != "offline" or output["plan"]["hash"] != output["plan"]["plan_hash"]:
                    raise ValueError("invalid offline plan envelope")
            elif result.stdout or 'unknown service "missing-api"' not in result.stderr:
                raise ValueError("rejection did not preserve its reason/empty output")
        print("PASS: checksum, embedded source/version and extracted normal/rejected examples")
        print("Unsigned local candidate; no SBOM/provenance or external-user claim")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    verify(parser.parse_args().directory)
