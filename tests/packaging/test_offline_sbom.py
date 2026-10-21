import base64
import copy
import hashlib
from pathlib import Path
import sys
import unittest

SCRIPTS = Path(__file__).resolve().parents[2] / "scripts"
sys.path.insert(0, str(SCRIPTS))
from offline_sbom import build_sbom


class SBOMTests(unittest.TestCase):
    def setUp(self):
        self.info = {
            "GoVersion": "go1.26.9", "Path": "themisy/cmd/themisyctl",
            "Main": {"Path": "themisy", "Version": "(devel)"},
            "Deps": [{"Path": "gopkg.in/yaml.v3", "Version": "v3.0.1", "Sum": "h1:" + base64.b64encode(bytes(32)).decode()}],
            "Settings": [{"Key": key, "Value": value} for key, value in
                (("CGO_ENABLED", "0"), ("GOOS", "darwin"), ("GOARCH", "arm64"))],
        }
        self.binary = b"synthetic binary contents"
        self.commit = "a" * 40

    def build(self, info=None, binary=None):
        return build_sbom(info or self.info, binary or self.binary, "0.1.0-alpha.1", self.commit, "darwin/arm64", 1792368000)

    def test_actual_binary_checksum_source_and_module_scope(self):
        before = copy.deepcopy(self.info)
        document = self.build()
        self.assertEqual(document["spdxVersion"], "SPDX-2.3")
        self.assertEqual(document["documentDescribes"], ["SPDXRef-Themisyctl"])
        packages = {item["SPDXID"]: item for item in document["packages"]}
        main = packages["SPDXRef-Themisyctl"]
        self.assertEqual(main["checksums"], [{"algorithm": "SHA256", "checksumValue": hashlib.sha256(self.binary).hexdigest()}])
        self.assertEqual(packages["SPDXRef-Source"]["versionInfo"], self.commit)
        modules = [item for item in document["packages"] if item["SPDXID"].startswith("SPDXRef-Module-")]
        self.assertEqual(len(modules), 1)
        self.assertEqual(modules[0]["name"], "gopkg.in/yaml.v3")
        self.assertIn("h1:", modules[0]["sourceInfo"])
        self.assertNotIn("checksums", modules[0])  # h1 is not the zip/archive SHA256.
        self.assertEqual(modules[0]["externalRefs"][0]["referenceLocator"], "pkg:golang/gopkg.in/yaml.v3@v3.0.1")
        self.assertTrue(all(item["filesAnalyzed"] is False for item in document["packages"]))
        self.assertTrue(all(item["licenseConcluded"] == "NOASSERTION" for item in document["packages"]))
        self.assertEqual(before, self.info)
        ids = {"SPDXRef-DOCUMENT", *packages}
        for relation in document["relationships"]:
            self.assertIn(relation["spdxElementId"], ids)
            self.assertIn(relation["relatedSpdxElement"], ids)

    def test_deterministic_document_and_content_specific_namespace(self):
        self.assertEqual(self.build(), self.build())
        self.assertNotEqual(self.build()["documentNamespace"], self.build(binary=b"changed binary")["documentNamespace"])
        other = copy.deepcopy(self.info)
        other["Deps"][0]["Version"] = "v3.0.2"
        self.assertNotEqual(self.build()["documentNamespace"], self.build(info=other)["documentNamespace"])

    def test_versioned_replacement_records_actual_code_identity(self):
        self.info["Deps"][0]["Replace"] = {"Path": "example.com/fork/yaml", "Version": "v1.0.0", "Sum": "h1:" + base64.b64encode(bytes([1] * 32)).decode()}
        module = self.build()["packages"][-1]
        self.assertEqual(module["name"], "example.com/fork/yaml")
        self.assertIn("replaces declared module gopkg.in/yaml.v3@v3.0.1", module["sourceInfo"])

    def test_unknown_unversioned_or_inconsistent_metadata_refuses(self):
        mutations = [
            lambda item: item.update(Path="unrelated/command"),
            lambda item: item.update(GoVersion="devel goX"),
            lambda item: item["Main"].update(Replace={"Path": "../private"}),
            lambda item: item["Settings"].append({"Key": "CGO_ENABLED", "Value": "1"}),
            lambda item: item["Settings"][1].update(Value="linux"),
            lambda item: item["Settings"][0].update(Value="1"),
            lambda item: item["Deps"][0].update(Sum=""),
            lambda item: item["Deps"][0].update(Sum="h1:invalid"),
            lambda item: item["Deps"][0].update(Replace={"Path": "../private"}),
            lambda item: item["Deps"].append(copy.deepcopy(item["Deps"][0])),
        ]
        for mutation in mutations:
            info = copy.deepcopy(self.info)
            mutation(info)
            with self.assertRaises(ValueError):
                self.build(info=info)


if __name__ == "__main__":
    unittest.main()
