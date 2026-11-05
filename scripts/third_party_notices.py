"""Concatenate license texts of Go modules linked into a built binary plus the Go toolchain.

Reads the local module cache (no network). A module with no license file is listed as
MISSING and makes the generator fail so an incomplete notice file is never produced.
"""

import re
import subprocess
from pathlib import Path


def build(package, cwd, env):
    out = subprocess.check_output(["go", "list", "-deps", "-f", "{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}", package],
                                  cwd=cwd, env=env, text=True)
    modules = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) == 3 and not parts[0].startswith("themisy"):
            modules[(parts[0], parts[1])] = Path(parts[2])
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], cwd=cwd, env=env, text=True).strip())
    sections, missing = [], []
    for (path, version), directory in sorted(modules.items()):
        text = None
        for root in (directory, directory.parent):
            files = sorted(f for f in root.iterdir() if re.match(r"(?i)(licen[sc]e|copying)", f.name) and f.is_file()) if root.is_dir() else []
            if files:
                text = files[0].read_text(errors="ignore")
                break
        if text is None:
            missing.append(f"{path}@{version}")
        else:
            sections.append(f"== {path} {version} ==\n{text.strip()}\n")
    if missing:
        raise RuntimeError("no license file for: " + ", ".join(missing))
    sections.append("== Go standard library and toolchain ==\n" + (goroot / "LICENSE").read_text().strip() + "\n")
    header = "Third-party notices for components linked into this binary.\nGenerated from module license files; not legal advice.\n\n"
    return header + "\n".join(sections)
