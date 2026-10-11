#!/usr/bin/env python3
"""Run bounded, checkpointed Codex manager sessions in a dedicated worktree.

This supervises process lifetime, not the correctness of an agent's conclusions.
CI and independent review remain required; no automatic merge or deployment.
"""
from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

SCHEMA = {
    "type": "object",
    "properties": {
        "status": {"type": "string", "enum": ["progress", "idle", "blocked"]},
        "summary": {"type": "string"},
        "packet": {"type": "string"},
        "next_task": {"type": "string"},
        "evidence": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["status", "summary", "packet", "next_task", "evidence"],
    "additionalProperties": False,
}


def deadline(value: str) -> dt.datetime:
    result = dt.datetime.fromisoformat(value)
    if result.tzinfo is None:
        raise argparse.ArgumentTypeError("deadline needs an explicit timezone, e.g. +09:00")
    return result


def save(path: Path, value: dict) -> None:
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)


def validate_result(value: object) -> dict:
    if not isinstance(value, dict) or set(value) != set(SCHEMA["required"]):
        raise ValueError("manager output has an invalid shape")
    if value["status"] not in ("progress", "idle", "blocked"):
        raise ValueError("manager output has an invalid status")
    if not all(isinstance(value[key], str) for key in ("summary", "packet", "next_task")):
        raise ValueError("manager summary and next_task must be strings")
    if not isinstance(value["evidence"], list) or not all(isinstance(x, str) for x in value["evidence"]):
        raise ValueError("manager evidence must be a string array")
    return value


def validate_packet(root: Path, state_dir: Path, reference: str) -> dict:
    """Check recorded evidence consistency, not the truth of model assertions."""
    def evidence_path(value):
        if not isinstance(value, str) or not value:
            raise ValueError("missing evidence path")
        path = (state_dir / value).resolve()
        if not path.is_relative_to(state_dir.resolve()) or not path.is_file():
            raise ValueError("evidence must be a file inside state-dir")
        if not path.stat().st_size:
            raise ValueError("empty evidence file")
        return path

    def nonblank(value):
        return isinstance(value, str) and bool(value.strip())

    path = evidence_path(reference)
    packet = json.loads(path.read_text())
    if not isinstance(packet, dict):
        raise ValueError("invalid packet")
    for key in ("task_id", "objective", "implementer", "candidate_commit"):
        if not isinstance(packet.get(key), str) or not packet[key].strip():
            raise ValueError(f"missing packet {key}")
    if not isinstance(packet.get("acceptance"), list) or not packet["acceptance"] or not all(isinstance(x, str) and x.strip() for x in packet["acceptance"]):
        raise ValueError("packet needs explicit acceptance criteria")
    commit = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
    if packet["candidate_commit"] != commit:
        raise ValueError("packet does not describe current HEAD")
    if subprocess.run(["git", "-C", str(root), "diff", "--quiet", "HEAD"]).returncode:
        raise ValueError("uncommitted tracked changes invalidate candidate evidence")
    untracked = subprocess.check_output(["git", "-C", str(root), "ls-files", "--others", "--exclude-standard", "-z"], text=True)
    for name in filter(None, untracked.split("\0")):
        if not (root / name).resolve().is_relative_to(state_dir.resolve()):
            raise ValueError("untracked implementation files invalidate candidate evidence")
    product = packet.get("product_decision", {})
    if not isinstance(product, dict) or product.get("basis") not in ("accepted_issue", "independent_review") or not nonblank(product.get("reference")):
        raise ValueError("missing product acceptance basis")
    if product["basis"] == "independent_review" and (not nonblank(product.get("reviewer")) or not nonblank(product.get("proposer")) or product["reviewer"].strip() == product["proposer"].strip()):
        raise ValueError("product review must be independent")
    review = packet.get("review", {})
    if not isinstance(review, dict) or review.get("verdict") != "pass" or review.get("commit") != commit or not nonblank(review.get("reviewer")) or review["reviewer"].strip() == packet["implementer"].strip():
        raise ValueError("independent passing review of candidate required")
    evidence_path(review.get("log"))
    required = packet.get("required_checks", [])
    checks = packet.get("checks", [])
    if not isinstance(required, list) or not all(isinstance(x, str) for x in required) or not isinstance(checks, list):
        raise ValueError("invalid check records")
    names = set()
    for check in checks:
        if not isinstance(check, dict) or not isinstance(check.get("name"), str) or check["name"] in names or not nonblank(check.get("command")) or check.get("commit") != commit or type(check.get("exit_code")) is not int or check["exit_code"] != 0:
            raise ValueError("checks must uniquely pass on the reviewed commit")
        names.add(check["name"])
        evidence_path(check.get("log"))
    if not (set(required) | {"format", "unit", "race", "lint"}).issubset(names):
        raise ValueError("required checks missing")
    ci = packet.get("ci", {})
    if not isinstance(ci, dict) or ci.get("status") not in ("passed", "not_run") or ci.get("commit") != commit:
        raise ValueError("CI must describe this candidate, with failures unresolved")
    if ci["status"] == "passed" and not nonblank(ci.get("url")):
        raise ValueError("passed CI needs run URL")
    return {"packet_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "candidate_commit": commit, "verification": "ci_passed" if ci["status"] == "passed" else "local_only"}


def group_alive(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False


def stop_process(process: subprocess.Popen) -> None:
    # Always kill remaining group members, even if the leader has already exited.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait()
    if process.stdin is not None:
        try:
            process.stdin.close()
        except BrokenPipeError:
            pass


def interrupted(signum, _frame):
    raise KeyboardInterrupt(f"received signal {signum}")


def prompt(root: Path, state_dir: Path, stop_at: dt.datetime, previous: dict) -> str:
    return f"""Act as the manager for one bounded Themisy development cycle.
Working tree: {root}
Hard stop: {stop_at.isoformat()}
Read docs/agent-operations/README.md, README.md, docs/implementation-plan.md,
accepted ADRs and threat model before choosing work. Read checkpoint files in
{state_dir} and git status/history so interrupted work is reviewed before reuse.
Previous cycle: {json.dumps(previous, ensure_ascii=False)}

Have a product proposer and a separate product reviewer approve new ideas, or
record the already-accepted Issue being implemented. Use subagents for independent
implementation and implementation review. Keep the fixed ECS MVP goal. Select one bounded task with acceptance
criteria; persist its scope and evidence under the local state directory before
editing. Implement, test and independently review it, repair at most three times,
then checkpoint. Tie review and test evidence to the exact diff/commit reviewed.
Do not treat missing/skipped tests as passing. Check CI when available. A review
verdict alone does not prove that CI passed. Preserve existing user changes.

Do not merge, push, publish, deploy, contact others, use production credentials,
redeem usage-reset credits, modify user settings, or weaken safety gates. Local
commits of your own tested changes are allowed. External staging tests remain
unverified unless an explicitly authorized isolated environment is available.
Do not start another manager or modify this supervisor, state.json or cycle result files.
Write task packets and evidence logs inside the local state directory. Read the
packet format in docs/agent-operations/packet-format.md; return its relative path
in the packet field whenever work was completed (including the last idle cycle).
Commit all implementation changes before reviewing and testing the candidate.
Use packet="" only when no task was completed. Progress requires a passing packet.
A locally verified packet may record CI not_run; never claim remote CI ran for
a local commit or reuse another commit's green CI. Pending remote verification
is a remaining task, not a released product.
The supervisor does not enforce filesystem/security boundaries beyond Codex's
sandbox, so keep all work within this explicit task scope.

Write an incremental checkpoint before expensive work. Stop starting new work
fifteen minutes before the hard stop and leave a self-contained handoff. Return
progress only if useful bounded work completed and an actionable next task
remains; idle if accepted work is exhausted; blocked if continuation needs user
input or unavailable infrastructure AND no independent accepted task is runnable.
A blocked packet does not block unrelated accepted work. Never invent work to consume the window.
"""


def run(args: argparse.Namespace) -> int:
    root = args.worktree.resolve()
    if not root.is_dir():
        raise ValueError("worktree directory does not exist")
    git_common = subprocess.check_output(
        ["git", "-C", str(root), "rev-parse", "--git-common-dir"], text=True
    ).strip()
    common = (root / git_common).resolve()
    state_dir = args.state_dir.resolve()
    state_dir.mkdir(parents=True, exist_ok=True)
    # Repository-wide lock prevents two managers even with different state dirs.
    with (common / "themisy-agent-cycle.lock").open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("another agent-cycle manager owns this repository")
        state_file = state_dir / "state.json"
        state = json.loads(state_file.read_text()) if state_file.exists() else {"cycles": []}
        if state.get("worktree", str(root)) != str(root):
            raise ValueError("checkpoint belongs to another worktree")
        if state.get("pgid") and group_alive(state["pgid"]):
            raise ValueError("previous process group still exists; stop it before resuming")
        if state.get("status") in ("running", "interrupted", "timeout"):
            if not args.resume_interrupted:
                raise ValueError("interrupted cycle: inspect work and confirm stopped workers, then use --resume-interrupted")
            state["interrupted_cycle"] = state.get("active_cycle")
        state.update(worktree=str(root), deadline=args.until.isoformat(), status="ready")
        save(state_file, state)
        schema = state_dir / "result-schema.json"
        save(schema, SCHEMA)
        started_cycles = 0
        while True:
            if args.max_cycles and started_cycles >= args.max_cycles:
                state["status"] = "cycle_limit"
                save(state_file, state)
                return 0
            remaining = (args.until - dt.datetime.now(dt.timezone.utc)).total_seconds()
            if remaining <= args.wrap_up_seconds:
                state["status"] = "deadline"
                save(state_file, state)
                return 0
            index = max([entry["index"] for entry in state["cycles"]] + [state.get("active_cycle", 0)]) + 1
            output = state_dir / f"cycle-{index:04d}.json"
            events = state_dir / f"cycle-{index:04d}.jsonl"
            errors = state_dir / f"cycle-{index:04d}.stderr"
            previous = state["cycles"][-1] if state["cycles"] else {}
            command = [args.codex, "exec", "--approve-for-me", "-C", str(root),
                       "--add-dir", str(state_dir), "--json", "--output-schema", str(schema), "--output-last-message", str(output), "-"]
            state.update(status="running", active_cycle=index)
            save(state_file, state)
            print(f"Starting cycle {index}; deadline {args.until.isoformat()}", flush=True)
            with events.open("w") as stdout, errors.open("w") as stderr:
                cycle_prompt = prompt(root, state_dir, args.until, previous)
                if args.task_file:
                    cycle_prompt += "\nAuthorized task for this run (stay within its scope):\n" + args.task_file.read_text()
                started_cycles += 1
                remaining = (args.until - dt.datetime.now(dt.timezone.utc)).total_seconds()
                if remaining <= args.wrap_up_seconds:
                    state["status"] = "deadline"
                    save(state_file, state)
                    return 0
                process = None
                try:
                    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr,
                                               text=True, start_new_session=True, pass_fds=(lock.fileno(),))
                    state["pgid"] = process.pid
                    save(state_file, state)
                    remaining = (args.until - dt.datetime.now(dt.timezone.utc)).total_seconds()
                    process.communicate(cycle_prompt,
                                        timeout=max(0, min(args.cycle_seconds, remaining)))
                except subprocess.TimeoutExpired:
                    state["status"] = "timeout"
                    return 2
                except BaseException:
                    state["status"] = "interrupted"
                    raise
                finally:
                    # Cleanup also covers failed checkpoint persistence immediately
                    # after spawn and a normal leader exit with live descendants.
                    if process is not None:
                        stop_process(process)
                    save(state_file, state)
            state.pop("pgid", None)
            if process.returncode:
                # Includes quota/auth errors. Never retry in a loop or redeem credits.
                state.update(status="execution_failed", exit_code=process.returncode)
                save(state_file, state)
                return 2
            try:
                result = validate_result(json.loads(output.read_text()))
                if result["status"] == "progress" and not result["packet"]:
                    raise ValueError("progress requires an evidence packet")
                if result["packet"]:
                    result.update(validate_packet(root, state_dir, result["packet"]))
            except (OSError, ValueError) as error:
                state.update(status="invalid_result", error=str(error))
                save(state_file, state)
                return 2
            result.update(index=index, completed_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                          events=str(events), output=str(output))
            state["cycles"].append(result)
            state["status"] = result["status"]
            save(state_file, state)
            print(result["summary"], flush=True)
            if result["status"] != "progress":
                return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worktree", type=Path, required=True)
    parser.add_argument("--state-dir", type=Path, required=True, help="local logs/checkpoints; do not commit")
    parser.add_argument("--until", type=deadline, required=True, help="ISO datetime including timezone")
    parser.add_argument("--cycle-seconds", type=int, default=1800)
    parser.add_argument("--wrap-up-seconds", type=int, default=900)
    parser.add_argument("--codex", default="codex")
    parser.add_argument("--task-file", type=Path, help="trusted task scope supplied by the operator")
    parser.add_argument("--max-cycles", type=int, default=0, help="optional per-invocation cycle cap; 0 means deadline only")
    parser.add_argument("--resume-interrupted", action="store_true",
                        help="after inspecting interrupted changes and confirming old workers stopped")
    args = parser.parse_args()
    if args.cycle_seconds <= 0 or args.wrap_up_seconds < 0 or args.max_cycles < 0:
        parser.error("cycle-seconds must be positive and wrap-up-seconds nonnegative")
    signal.signal(signal.SIGTERM, interrupted)
    try:
        return run(args)
    except KeyboardInterrupt:
        return 130
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
