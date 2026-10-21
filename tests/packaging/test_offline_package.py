"""Negative controls using an explicitly supplied trusted native candidate."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[2] / "scripts"
sys.path.insert(0, str(SCRIPTS))
from offline_sbom import build_sbom
spec = importlib.util.spec_from_file_location("verify_offline", SCRIPTS / "verify-offline-package.py")
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


class NativeCandidateTamperingTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = os.environ.get("THEMISY_OFFLINE_CANDIDATE_TEST_DIRECTORY")
        if not directory:
            raise unittest.SkipTest("requires an explicitly built trusted native candidate")
        directory = Path(directory)
        _, cls.name = (directory / "SHA256SUMS").read_text().strip().split("  ", 1)
        with tarfile.open(directory / cls.name) as archive:
            cls.records = [(copy.copy(member), archive.extractfile(member).read()) for member in archive.getmembers() if member.isfile()]
        cls.prefix = cls.records[0][0].name.split("/", 1)[0] + "/"

    def mutated(self, directory, mode):
        contents = {record.name: data for record, data in self.records}
        manifest_name, sbom_name, info_name = (self.prefix + name for name in ("build-manifest.json", "sbom.spdx.json", "build-info.json"))
        manifest = json.loads(contents[manifest_name])
        sbom = json.loads(contents[sbom_name])
        if mode == "wrong-source":
            next(item for item in sbom["packages"] if item["SPDXID"] == "SPDXRef-Source")["versionInfo"] = "b" * 40
        elif mode == "missing-module":
            module = next(item for item in sbom["packages"] if item["SPDXID"].startswith("SPDXRef-Module-"))
            sbom["packages"].remove(module)
            sbom["relationships"] = [relation for relation in sbom["relationships"] if relation["relatedSpdxElement"] != module["SPDXID"]]
        elif mode == "fake-build-info":
            info = json.loads(contents[info_name])
            info["GoVersion"] = "go1.26.8"
            contents[info_name] = json.dumps(info).encode()
            manifest["go_version"] = info["GoVersion"]
            sbom = build_sbom(info, contents[self.prefix + "themisyctl"], manifest["version"],
                manifest["source_commit"], manifest["platform"], manifest["source_date_epoch"])
        else:
            sbom["name"] = "changed document"
        contents[sbom_name] = json.dumps(sbom).encode()
        if mode != "wrong-sbom-hash":
            manifest["sbom"]["sha256"] = hashlib.sha256(contents[sbom_name]).hexdigest()
        contents[manifest_name] = json.dumps(manifest).encode()
        archive_path = directory / self.name
        with tarfile.open(archive_path, "w:gz") as archive:
            for original, _ in self.records:
                member = copy.copy(original)
                data = contents[member.name]
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        # Update the outer checksum deliberately: the deeper binding check must
        # detect the inconsistency rather than relying on a stale SHA256SUMS.
        (directory / "SHA256SUMS").write_text(hashlib.sha256(archive_path.read_bytes()).hexdigest() + "  " + self.name + "\n")

    def test_rehashed_sbom_cannot_change_source_or_drop_dependencies(self):
        for mode in ("wrong-source", "missing-module"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                directory = Path(temp)
                self.mutated(directory, mode)
                with self.assertRaisesRegex(ValueError, "SBOM binary/source/dependency binding mismatch"):
                    verifier.verify(directory)

    def test_rehashed_metadata_cannot_disagree_with_binary_header(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.mutated(directory, "fake-build-info")
            with self.assertRaisesRegex(ValueError, "stored Go build metadata does not match the binary"):
                verifier.verify(directory)

    def test_sbom_requires_its_declared_checksum(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.mutated(directory, "wrong-sbom-hash")
            with self.assertRaisesRegex(ValueError, "SBOM checksum mismatch"):
                verifier.verify(directory)


if __name__ == "__main__":
    unittest.main()
