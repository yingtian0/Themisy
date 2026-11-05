"""List license classifications of Go modules linked into the given packages.

Heuristic aid for human review, not legal advice. Classifies only well-known
license texts; anything else is reported as UNCLASSIFIED or NO-LICENSE-FILE and must
be reviewed by a person. Reads the local module cache; no network access.
"""

import json
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path


def classify(text):
    t = " ".join(text.lower().split())
    if "apache license" in t and "version 2.0" in t:
        return "Apache-2.0"
    if "mozilla public license" in t:
        return "MPL-2.0"
    if "gnu general public license" in t or "gnu lesser general public license" in t or "gnu affero" in t:
        return "GPL-family"
    if "permission is hereby granted, free of charge" in t:
        return "MIT"
    if "redistribution and use in source and binary forms" in t:
        return "BSD-3-Clause" if "neither the name" in t else "BSD-2-Clause"
    if "permission to use, copy, modify" in t:
        return "ISC"
    return "UNCLASSIFIED"


def inventory(packages):
    out = subprocess.run(["go", "list", "-deps", "-f", "{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}", *packages],
                         capture_output=True, text=True, check=True).stdout
    rows = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) != 3 or parts[0].startswith("themisy"):
            continue
        path, version, directory = parts
        verdict = "NO-LICENSE-FILE"
        for root in (Path(directory), Path(directory).parent):
            files = [f for f in root.iterdir() if re.match(r"(?i)(licen[sc]e|copying)", f.name) and f.is_file()] if root.is_dir() else []
            if files:
                verdict = classify(files[0].read_text(errors="ignore"))
                break
        rows[(path, version)] = verdict
    return rows


if __name__ == "__main__":
    rows = inventory(sys.argv[1:] or ["./cmd/..."])
    print(json.dumps({"counts": Counter(rows.values()),
                      "needs_review": sorted(f"{p}@{v}: {c}" for (p, v), c in rows.items()
                                             if c not in {"Apache-2.0", "MIT", "BSD-3-Clause", "BSD-2-Clause", "ISC"})}, indent=2))
