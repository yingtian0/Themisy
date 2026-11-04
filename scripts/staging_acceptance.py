"""Read-only preflight and IAM-denial checks for the dedicated ECS staging gate.

Runs only the AWS CLI's read-only calls (sts get-caller-identity, ecs
describe-services, iam simulate-principal-policy). It never writes to AWS. Any
error, missing field or unexpected decision is INCONCLUSIVE or FAIL, never PASS.
Use credentials of a dedicated read-only verifier, never CI/Agent/Runner ones.
"""

import argparse
import hashlib
import json
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

PASS, FAIL, INCONCLUSIVE = "PASS", "FAIL", "INCONCLUSIVE"
DENIED = {"implicitDeny", "explicitDeny"}
REQUIRED = ("expected_account_id", "region", "cluster", "service")


def aws(*args):
    out = subprocess.run(["aws", *args, "--output", "json"], capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip().splitlines()[-1] if out.stderr.strip() else "aws failed")
    return json.loads(out.stdout or "{}")


def check(name, status, detail):
    return {"check": name, "status": status, "detail": detail}


def load(path):
    cfg = json.loads(Path(path).read_text())
    missing = [k for k in REQUIRED if not cfg.get(k)]
    if missing:
        raise ValueError("config missing: " + ", ".join(missing))
    if not re.fullmatch(r"\d{12}", cfg["expected_account_id"]):
        raise ValueError("expected_account_id must be 12 digits")
    return cfg


def preflight(cfg):
    results = []
    for field in ("cluster", "service"):
        bad = re.search(r"prod|production|live", cfg[field], re.I)
        results.append(check(f"{field}-name-not-production", FAIL if bad else PASS, cfg[field]))
    try:
        account = aws("sts", "get-caller-identity")["Account"]
        results.append(check("account-matches-staging", PASS if account == cfg["expected_account_id"] else FAIL,
                             "caller account " + account))
    except Exception as exc:
        results.append(check("account-matches-staging", INCONCLUSIVE, str(exc)))
    try:
        desc = aws("ecs", "describe-services", "--region", cfg["region"], "--cluster", cfg["cluster"],
                   "--services", cfg["service"])
        found = [s for s in desc.get("services", []) if s.get("status") == "ACTIVE"]
        results.append(check("service-active", PASS if found else FAIL, f"{len(found)} active"))
    except Exception as exc:
        results.append(check("service-active", INCONCLUSIVE, str(exc)))
    return results


def simulate(principal, actions, resource):
    data = aws("iam", "simulate-principal-policy", "--policy-source-arn", principal,
               "--action-names", *actions, "--resource-arns", resource)
    return {r["EvalActionName"]: r["EvalDecision"] for r in data.get("EvaluationResults", [])}


def iam_denial(cfg):
    results = []
    service_arn = cfg.get("service_arn")
    if not service_arn:
        return [check("config", INCONCLUSIVE, "service_arn required")]
    actions = cfg.get("denied_actions") or ["ecs:UpdateService", "ecs:RegisterTaskDefinition", "iam:PassRole"]
    for principal in cfg.get("denied_principals", []):
        try:
            decisions = simulate(principal, actions, service_arn)
            missing = [a for a in actions if a not in decisions]
            allowed = sorted(a for a, d in decisions.items() if d not in DENIED)
            if missing:
                results.append(check("direct-write-denied " + principal, INCONCLUSIVE, "no decision for " + ",".join(missing)))
            else:
                results.append(check("direct-write-denied " + principal, FAIL if allowed else PASS,
                                     "allowed: " + ",".join(allowed) if allowed else "all denied"))
        except Exception as exc:
            results.append(check("direct-write-denied " + principal, INCONCLUSIVE, str(exc)))
    runner = cfg.get("runner_role_arn")
    for resource in cfg.get("runner_denied_resources", []):
        try:
            decisions = simulate(runner, ["ecs:UpdateService"], resource) if runner else {}
            ok = decisions.get("ecs:UpdateService") in DENIED
            status = INCONCLUSIVE if not decisions else (PASS if ok else FAIL)
            results.append(check("runner-outside-allowlist-denied " + resource, status, str(decisions)))
        except Exception as exc:
            results.append(check("runner-outside-allowlist-denied " + resource, INCONCLUSIVE, str(exc)))
    if not cfg.get("denied_principals"):
        results.append(check("denied-principals", INCONCLUSIVE, "no CI/Agent principals configured"))
    return results


def overall(results):
    statuses = {r["status"] for r in results}
    return FAIL if FAIL in statuses else INCONCLUSIVE if INCONCLUSIVE in statuses or not results else PASS


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("command", choices=("preflight", "iam-denial"))
    p.add_argument("config")
    p.add_argument("--evidence", required=True, help="new JSON evidence file path")
    a = p.parse_args(argv)
    out = Path(a.evidence)
    if out.exists():
        p.error("evidence file must not exist")
    cfg = load(a.config)
    results = preflight(cfg) if a.command == "preflight" else iam_denial(cfg)
    doc = {"command": a.command, "time": datetime.now(timezone.utc).isoformat(), "config_sha256":
           hashlib.sha256(Path(a.config).read_bytes()).hexdigest(), "overall": overall(results), "results": results}
    out.write_text(json.dumps(doc, indent=2) + "\n")
    for r in results:
        print(f"{r['status']:13} {r['check']}: {r['detail']}")
    print("OVERALL", doc["overall"])
    return 0 if doc["overall"] == PASS else 1


if __name__ == "__main__":
    sys.exit(main())
