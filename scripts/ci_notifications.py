#!/usr/bin/env python3
"""Read completion receipts; acknowledge only after delivering to the chat.

No inbound listener, model execution, source checkout, merge, or deploy. gh uses
its existing local Actions-read authentication. Remote strings are data only.
"""
import argparse
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import urlencode

REPO = "yingtian0/Themisy"
SIGNAL = ".github/workflows/ci-completed.yml"
SOURCE = ".github/workflows/ci.yml"
CONCLUSIONS = {"success", "failure", "cancelled", "timed_out", "action_required",
               "neutral", "skipped", "stale", "startup_failure"}
TITLE = re.compile(r"CI completed ([1-9][0-9]*) attempt ([1-9][0-9]*)")


def api(path, paginate=False):
    command = ["gh", "api", "--hostname", "github.com"]
    if paginate:
        command += ["--paginate", "--slurp"]
    try:
        result = subprocess.run(command + [f"repos/{REPO}/{path}"],
                                capture_output=True, text=True, timeout=60, check=True)
    except (OSError, subprocess.SubprocessError) as error:
        raise ValueError("GitHub Actions read failed; notification state was not advanced") from error
    return json.loads(result.stdout)


def timestamp(value):
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("since must include a timezone")
    return parsed.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def source_notification(run, run_id, attempt):
    if (run.get("id") != run_id or run.get("run_attempt") != attempt
            or run.get("repository", {}).get("full_name") != REPO
            or run.get("path") != SOURCE or run.get("status") != "completed"
            or run.get("conclusion") not in CONCLUSIONS
            or not re.fullmatch(r"[0-9a-f]{40}", run.get("head_sha", ""))):
        raise ValueError("source run does not match the completion receipt")
    return {"key": f"{run_id}:{attempt}", "run_id": run_id, "attempt": attempt,
            "conclusion": run["conclusion"], "head_sha": run["head_sha"],
            "url": f"https://github.com/{REPO}/actions/runs/{run_id}/attempts/{attempt}"}


def poll(state, fetch=api):
    query = urlencode({"per_page": 100, "event": "workflow_run", "created": ">=" + state["since"]})
    pages = fetch("actions/workflows/ci-completed.yml/runs?" + query, paginate=True)
    # Build the entire update first. A failed page/source read cannot acknowledge
    # or partially lose results. Re-running the observer also deduplicates here.
    pending = dict(state["pending"])
    latest = dt.datetime.fromisoformat(state["since"].replace("Z", "+00:00"))
    for page in pages:
        if page.get("total_count", 0) > 1000:
            raise ValueError("receipt backlog exceeds GitHub's 1000-result limit; state was not advanced")
        for receipt in page["workflow_runs"]:
            if receipt.get("event") != "workflow_run" or receipt.get("path") != SIGNAL:
                raise ValueError("unexpected completion observer")
            created = dt.datetime.fromisoformat(timestamp(receipt["created_at"]).replace("Z", "+00:00"))
            latest = max(latest, created)
            match = TITLE.fullmatch(receipt.get("display_title", ""))
            if not match:
                raise ValueError("invalid completion receipt")
            run_id, attempt = map(int, match.groups())
            key = f"{run_id}:{attempt}"
            if key in state["delivered"] or key in pending:
                continue
            run = fetch(f"actions/runs/{run_id}/attempts/{attempt}")
            pending[key] = source_notification(run, run_id, attempt)
    # Retain a day of overlap for delayed indexing; pending records and delivery
    # keys are independent of the query cursor. Old source reruns create new receipts.
    overlap = (latest - dt.timedelta(days=1)).isoformat().replace("+00:00", "Z")
    since = max(timestamp(state["since"]), overlap)
    return {**state, "pending": pending, "since": since}


def save(path, state):
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as output:
        json.dump(state, output, indent=2, sort_keys=True)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["poll", "ack"])
    parser.add_argument("--state", type=Path, required=True,
                        help="private per-chat state outside tracked source")
    parser.add_argument("--since", help="initial subscription time (RFC3339); required for a new state")
    parser.add_argument("--key", action="append", default=[], help="delivered run:attempt to acknowledge")
    args = parser.parse_args()
    args.state.parent.mkdir(parents=True, exist_ok=True)
    with args.state.with_suffix(".lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if args.state.exists():
            state = json.loads(args.state.read_text())
            if state.get("schema") != 1 or state.get("repository") != REPO:
                raise ValueError("invalid notification state")
        elif args.command == "poll" and args.since:
            state = {"schema": 1, "repository": REPO, "since": timestamp(args.since),
                     "pending": {}, "delivered": []}
        else:
            raise ValueError("initialize with poll --since before acknowledging")
        if args.command == "poll":
            state = poll(state)
        else:
            for key in args.key:
                if key not in state["pending"] and key not in state["delivered"]:
                    raise ValueError("cannot acknowledge an unknown notification")
            for key in args.key:
                state["pending"].pop(key, None)
                if key not in state["delivered"]:
                    state["delivered"].append(key)
        save(args.state, state)
        print(json.dumps({"notifications": list(state["pending"].values())}, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError) as error:
        print(f"ci-notifications: {error}", file=sys.stderr)
        sys.exit(1)
