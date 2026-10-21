"""Smoke-test a trusted, locally built offline candidate; never authenticate origin."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

from offline_sbom import build_sbom, sha256


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
        sbom_info = manifest.get("sbom", {})
        if sbom_info.get("file") != "sbom.spdx.json":
            raise ValueError("SBOM file is missing or unsupported")
        sbom_bytes = (root / "sbom.spdx.json").read_bytes()
        if sha256(sbom_bytes) != sbom_info.get("sha256"):
            raise ValueError("SBOM checksum mismatch")
        build_info = json.loads((root / "build-info.json").read_text())
        inspected_info = json.loads(subprocess.check_output(["go", "version", "-m", "-json", str(root / "themisyctl")],
            cwd=root, env=dict(os.environ, GOWORK="off", GOENV="off", GOTOOLCHAIN="local", GOPROXY="off")))
        if build_info != inspected_info or manifest["go_version"] != inspected_info["GoVersion"]:
            raise ValueError("stored Go build metadata does not match the binary")
        expected_sbom = build_sbom(inspected_info, (root / "themisyctl").read_bytes(),
            manifest["version"], manifest["source_commit"], manifest["platform"], manifest["source_date_epoch"])
        if json.loads(sbom_bytes) != expected_sbom:
            raise ValueError("SBOM binary/source/dependency binding mismatch")
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
        print("PASS: checksum, linked-module SBOM binding, embedded source/version and extracted normal/rejected examples")
        print("Unsigned local candidate; SBOM completeness, signed provenance and external-user verification remain unproved")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    verify(parser.parse_args().directory)
