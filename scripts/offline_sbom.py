"""SPDX 2.3 linked-Go-module SBOM for a local offline CLI candidate.

No network, license discovery, vulnerability assertion, or signing is performed.
Go h1 directory sums are source metadata, never archive SHA-256 checksums.
"""

import base64
from datetime import datetime, timezone
import hashlib
import json
import re
from urllib.parse import quote


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def package(spdx_id, name, version, download="NOASSERTION"):
    return {
        "SPDXID": spdx_id, "name": name, "versionInfo": version,
        "downloadLocation": download, "filesAnalyzed": False,
        "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
        "copyrightText": "NOASSERTION",
    }


def build_sbom(info, binary, version, commit, platform, epoch):
    """Describe the exact binary and the module metadata embedded by Go."""
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("SBOM source commit must be a full Git SHA")
    if info.get("Path") != "themisy/cmd/themisyctl" or info.get("Main", {}).get("Path") != "themisy":
        raise ValueError("unexpected binary/main module identity")
    if info["Main"].get("Replace"):
        raise ValueError("main-module replacement is not supported")
    go_version = info.get("GoVersion", "")
    if not re.fullmatch(r"go\d+\.\d+(?:\.\d+)?(?:(?:rc|beta)\d+)?", go_version):
        raise ValueError("missing or unversioned Go toolchain")
    settings = {}
    for item in info.get("Settings", []):
        if item["Key"] in settings:
            raise ValueError("duplicate Go build setting")
        settings[item["Key"]] = item["Value"]
    if platform not in ("darwin/arm64", "linux/amd64") or platform != settings.get("GOOS", "") + "/" + settings.get("GOARCH", "") or settings.get("CGO_ENABLED") != "0":
        raise ValueError("SBOM platform/static build settings mismatch")
    digest = sha256(binary)
    binary_id, source_id = "SPDXRef-Themisyctl", "SPDXRef-Source"
    binary_package = package(binary_id, "themisyctl", version)
    binary_package.update({
        "packageFileName": "./themisyctl",
        "checksums": [{"algorithm": "SHA256", "checksumValue": digest}],
        "sourceInfo": f"Themisy source commit {commit}; platform {platform}; offline planning only.",
    })
    packages = [binary_package, package(source_id, "Themisy source", commit,
        "git+https://github.com/yingtian0/Themisy.git@" + commit),
        package("SPDXRef-GoStdlib", "Go standard library", go_version),
        package("SPDXRef-GoCompiler", "Go toolchain", go_version)]
    packages[2]["sourceInfo"] = "Bundled Go standard library identified by the binary GoVersion; individual standard-library files and vendored components are not decomposed."
    relationships = [
        {"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": binary_id},
        {"spdxElementId": binary_id, "relationshipType": "GENERATED_FROM", "relatedSpdxElement": source_id},
        {"spdxElementId": binary_id, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": "SPDXRef-GoStdlib"},
        {"spdxElementId": "SPDXRef-GoCompiler", "relationshipType": "BUILD_TOOL_OF", "relatedSpdxElement": binary_id},
    ]
    seen = set()
    for declared in sorted(info.get("Deps") or [], key=lambda item: item["Path"]):
        effective = declared.get("Replace") or declared
        path, module_version, content_sum = (effective.get(key, "") for key in ("Path", "Version", "Sum"))
        if not path or not module_version.startswith("v") or not content_sum.startswith("h1:"):
            raise ValueError("unversioned/local/missing-sum dependency is not supported")
        try:
            if len(base64.b64decode(content_sum[3:], validate=True)) != 32:
                raise ValueError("invalid Go module sum")
        except Exception as error:
            raise ValueError("invalid Go module sum") from error
        identity = path + "@" + module_version
        if identity in seen:
            raise ValueError("duplicate effective module identity")
        seen.add(identity)
        module_id = "SPDXRef-Module-" + sha256(identity.encode())
        item = package(module_id, path, module_version)
        item["externalRefs"] = [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
            "referenceLocator": "pkg:golang/" + quote(path, safe="/") + "@" + quote(module_version, safe="")}]
        item["sourceInfo"] = "Go directory content sum (not a download archive checksum): " + content_sum
        if declared.get("Replace"):
            item["sourceInfo"] += "; replaces declared module " + declared["Path"] + "@" + declared.get("Version", "")
        packages.append(item)
        relationships.append({"spdxElementId": binary_id, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": module_id})
    document = {
        "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
        "name": "themisyctl-" + version + "-" + platform.replace("/", "-"),
        "creationInfo": {"created": datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "creators": ["Tool: themisy-offline-packager-v1"],
            "comment": "Timestamp normalized to source_date_epoch for reproducibility; actual build time and authenticated provenance are not asserted."},
        "documentDescribes": [binary_id], "packages": packages, "relationships": relationships,
        "comment": "Binary and linked Go module SBOM. License conclusions/suppliers are not determined. Standard-library files, vendored toolchain components and example assets are not decomposed. No vulnerability or provenance verification is asserted.",
    }

    content_hash = sha256(json.dumps(document, sort_keys=True, separators=(",", ":")).encode())
    document["documentNamespace"] = "https://github.com/yingtian0/Themisy/sbom/" + commit + "/" + platform + "/" + content_hash
    return document
