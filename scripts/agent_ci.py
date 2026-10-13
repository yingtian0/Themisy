"""Optional, explicit existing-PR CI gate; never creates or merges a PR."""
import datetime as dt
import json
import os
import re
import signal
import subprocess
import tempfile
import time

REQUIRED = {
    "Format, modules, vet, build, and test", "Race detector",
    "Planner determinism and safety scenarios", "PostgreSQL and Temporal recovery",
    "Go vulnerability scan",
}


def run_command(argv, worktree, remaining):
    # Check before spawn; regular files avoid inherited-pipe hangs after exit.
    timeout = min(60, remaining())
    with tempfile.TemporaryFile(mode="w+") as stdout, tempfile.TemporaryFile(mode="w+") as stderr:
        process = subprocess.Popen(argv, cwd=worktree, text=True,
                                   stdout=stdout, stderr=stderr, start_new_session=True)
        try:
            process.wait(timeout=min(timeout, remaining()))
        finally:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
            finally:
                # The leader can exit while a TERM-resistant descendant lives.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=2)
        stdout.seek(0)
        stderr.seek(0)
        output, error = stdout.read(), stderr.read()
    if process.returncode:
        raise subprocess.CalledProcessError(process.returncode, argv, output, error)
    return output.strip()


def verify_ci(worktree, pr_number, commit, deadline):
    if type(pr_number) is not int or pr_number < 1:
        raise ValueError("existing PR number must be positive")
    if deadline.tzinfo is None:
        raise ValueError("CI deadline needs timezone")
    if not re.fullmatch(r"[0-9a-f]{40,64}", commit):
        raise ValueError("full candidate commit required")

    def remaining():
        seconds = (deadline - dt.datetime.now(dt.timezone.utc)).total_seconds()
        if seconds <= 0:
            raise TimeoutError("CI deadline reached")
        return seconds

    def run(*argv):
        return run_command(list(argv), worktree, remaining)

    def git(*args):
        return run("git", *args)

    origin = git("remote", "get-url", "origin")
    push_urls = git("remote", "get-url", "--push", "--all", "origin").splitlines()
    if len(push_urls) != 1:
        raise ValueError("origin must have exactly one push destination")
    push_origin = push_urls[0]
    def repository(url):
        match = re.fullmatch(r"(?:https://|ssh://git@|git@)([A-Za-z0-9.-]+)[/:]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?", url)
        if not match:
            raise ValueError("unsupported origin URL")
        return match.groups()
    host, repo = repository(origin)
    if repository(push_origin) != (host, repo):
        raise ValueError("origin push repository differs")
    branch = git("symbolic-ref", "--quiet", "--short", "HEAD")
    if branch in ("main", "master") or git("rev-parse", "HEAD") != commit:
        raise ValueError("candidate must be current non-main branch HEAD")

    def api(path, *args):
        return json.loads(run("gh", "api", "--hostname", host, path, *args))

    def check_pr(require_commit=False):
        pr = api(f"repos/{repo}/pulls/{pr_number}")
        if (pr.get("state") != "open" or pr.get("merged") or
                (pr.get("head", {}).get("repo") or {}).get("full_name") != repo or
                (pr.get("base", {}).get("repo") or {}).get("full_name") != repo or
                pr["head"].get("ref") != branch or pr["base"].get("ref") == branch):
            raise ValueError("PR must be open, same-origin, and target current non-base branch")
        if require_commit and pr["head"].get("sha") != commit:
            raise ValueError("PR head changed from verified candidate")
        return pr

    previous_sha = check_pr()["head"].get("sha")
    # Explicit immutable source avoids pushing a concurrently moved local HEAD.
    if git("rev-parse", "HEAD") != commit or git("symbolic-ref", "--quiet", "--short", "HEAD") != branch:
        raise ValueError("local candidate changed before push")
    run("git", "push", "--no-mirror", "--no-follow-tags", "--recurse-submodules=no", "origin", f"{commit}:refs/heads/{branch}")
    while True:
        observed_sha = check_pr()["head"].get("sha")
        if observed_sha == commit:
            break
        if not previous_sha or observed_sha != previous_sha:
            raise ValueError("PR head changed from verified candidate")
        # GitHub's PR projection can briefly lag a successful push.
        time.sleep(min(10, remaining()))
    while True:
        check_pr(require_commit=True)
        pages = api(f"repos/{repo}/commits/{commit}/check-runs?per_page=100", "--paginate", "--slurp")
        latest = {}
        for page in pages:
            for check in page.get("check_runs", []):
                name = check.get("name")
                if name not in REQUIRED or check.get("head_sha") != commit or check.get("app", {}).get("slug") != "github-actions":
                    continue
                if name not in latest or check["id"] > latest[name]["id"]:
                    latest[name] = check
        for name, check in latest.items():
            if check.get("status") == "completed" and check.get("conclusion") != "success":
                raise ValueError(f"CI failed: {name}: {check.get('conclusion')}")
        if REQUIRED.issubset(latest) and all(c.get("status") == "completed" and c.get("conclusion") == "success" for c in latest.values()):
            pr = check_pr(require_commit=True)
            return {"status": "passed", "commit": commit, "url": pr["html_url"],
                    "checks": {name: latest[name].get("html_url") for name in sorted(REQUIRED)}}
        time.sleep(min(10, remaining()))
