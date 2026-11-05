"""Thin CI/CD gate around themisyctl. It never writes to a cloud and never trusts CI results.

Success is exactly one thing: the Control Plane reports the Run SUCCEEDED (which the
Workflow only reaches after signed runtime/health verification). A CI job passing, a
sync completing or a caller-supplied flag is not deployment success. Every error,
unknown status, timeout or unparsable output exits non-zero.
"""

import argparse
import json
import os
import subprocess
import sys
import time

OK, FAILED, PENDING, ERROR = 0, 1, 3, 2
TERMINAL_BAD = {"FAILED", "BLOCKED", "ESCALATED", "CANCELLED"}
WAITING = {"PENDING", "RUNNING", "PAUSED", "WAITING_APPROVAL"}


def ctl(args, binary):
    out = subprocess.run([binary, *args], capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip() or "themisyctl failed")
    return json.loads(out.stdout)


def status(run_id, binary):
    run = ctl(["status", run_id], binary)
    if not isinstance(run, dict) or not isinstance(run.get("status"), str):
        raise RuntimeError("unexpected status response")
    return run


def wait(run_id, binary, timeout, interval, sleep=time.sleep, clock=time.monotonic):
    deadline = clock() + timeout
    while True:
        run = status(run_id, binary)
        state = run["status"]
        if state == "SUCCEEDED":
            return OK, run
        if state in TERMINAL_BAD:
            return FAILED, run
        if state not in WAITING:
            raise RuntimeError("unknown run status " + state)
        if clock() >= deadline:
            return PENDING, run
        sleep(interval)


def verify_plan(run, plan_hash, environment):
    if run["status"] != "SUCCEEDED":
        return False, "run is " + run["status"]
    if run.get("plan", {}).get("hash") != plan_hash:
        return False, "run executed a different plan hash"
    if environment and run.get("environment") != environment:
        return False, "run environment differs"
    return True, "succeeded run matches plan"


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--themisyctl", default=os.environ.get("THEMISYCTL", "themisyctl"))
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("submit"); s.add_argument("intent")
    w = sub.add_parser("wait"); w.add_argument("run_id")
    w.add_argument("--timeout", type=int, default=1800); w.add_argument("--interval", type=int, default=15)
    v = sub.add_parser("verify"); v.add_argument("run_id"); v.add_argument("--plan-hash", required=True)
    v.add_argument("--environment")
    a = p.parse_args(argv)
    try:
        if a.cmd == "submit":
            run = ctl(["start", a.intent], a.themisyctl)
            if not run.get("id"):
                raise RuntimeError("response has no run id")
            print(run["id"])
            return OK
        if a.cmd == "wait":
            code, run = wait(a.run_id, a.themisyctl, a.timeout, a.interval)
            print(run["status"])
            return code
        ok, why = verify_plan(status(a.run_id, a.themisyctl), a.plan_hash, a.environment)
        print(why)
        return OK if ok else FAILED
    except Exception as exc:
        print("error:", exc, file=sys.stderr)
        return ERROR


if __name__ == "__main__":
    sys.exit(main())
