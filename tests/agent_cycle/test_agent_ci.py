import datetime as dt
import importlib.util
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("agent_ci", Path(__file__).resolve().parents[2] / "scripts" / "agent_ci.py")
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)


class CITests(unittest.TestCase):
    def setUp(self):
        self.commit = "a" * 40
        self.branch = "work"
        self.pr = {"state": "open", "merged": False, "head": {"repo": {"full_name": "owner/repo"}, "ref": "work", "sha": self.commit}, "base": {"repo": {"full_name": "owner/repo"}, "ref": "main"}, "html_url": "https://github.com/owner/repo/pull/1"}
        self.checks = [{"id": i, "name": n, "head_sha": self.commit, "app": {"slug": "github-actions"}, "status": "completed", "conclusion": "success", "html_url": f"https://github.com/check/{i}"} for i, n in enumerate(sorted(ci.REQUIRED))]
        self.calls = []
        self.pr_reads = 0
        self.change_head = False
        self.push_url = "git@github.com:owner/repo.git"
        self.head_sequence = []
        self.until = dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=1)

    def command(self, args, **kwargs):
        self.calls.append(args)
        self.assertGreater(kwargs["timeout"], 0)
        self.assertLessEqual(kwargs["timeout"], 60)
        if args[:3] == ["git", "remote", "get-url"]:
            if "--push" in args:
                return self.push_url
            return "git@github.com:owner/repo.git"
        if args[:2] == ["git", "symbolic-ref"]:
            return self.branch
        if args[:2] == ["git", "rev-parse"]:
            return self.commit
        if args[:2] == ["git", "push"]:
            return ""
        if args[0] == "gh" and "/pulls/" in args[4]:
            self.pr_reads += 1
            if self.change_head and self.pr_reads == 3:
                self.pr["head"]["sha"] = "b" * 40
            if self.head_sequence:
                self.pr["head"]["sha"] = self.head_sequence.pop(0)
            return json.dumps(self.pr)
        if args[0] == "gh" and "/check-runs?" in args[4]:
            return json.dumps([{"check_runs": self.checks}])
        self.fail(f"unexpected subprocess {args}")

    def invoke(self):
        def command(args, worktree, remaining):
            return self.command(args, timeout=min(60, remaining()))
        with patch.object(ci, "run_command", side_effect=command):
            return ci.verify_ci("/unused", 1, self.commit, self.until)

    def test_expired_command_never_spawns(self):
        with patch.object(ci.subprocess, "Popen") as spawn:
            with self.assertRaises(TimeoutError):
                ci.run_command(["git", "push"], "/unused", lambda: (_ for _ in ()).throw(TimeoutError()))
            spawn.assert_not_called()

    def test_real_command_descendants_cleaned_on_exit_timeout_and_interrupt(self):
        for mode in ("exit", "timeout", "interrupt"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                pidfile = Path(directory) / "child.pid"
                child = ("import os,signal,time; from pathlib import Path; "
                         "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                         f"Path({str(pidfile)!r}).write_text(str(os.getpid())); time.sleep(30)")
                parent = ("import subprocess,sys,time; from pathlib import Path\n"
                          f"subprocess.Popen([sys.executable, '-c', {child!r}])\n"
                          f"while not Path({str(pidfile)!r}).exists(): time.sleep(.01)\n"
                          + ("print('done')\n" if mode == "exit" else "time.sleep(30)\n"))
                calls = 0
                def remaining():
                    nonlocal calls
                    calls += 1
                    if mode == "interrupt" and calls == 2:
                        end = time.monotonic() + 3
                        while not pidfile.exists() and time.monotonic() < end:
                            time.sleep(.01)
                        raise KeyboardInterrupt()
                    return 1 if mode == "timeout" else 5
                started = time.monotonic()
                try:
                    if mode == "exit":
                        self.assertEqual(ci.run_command([sys.executable, "-c", parent], directory, remaining), "done")
                    else:
                        with self.assertRaises(subprocess.TimeoutExpired if mode == "timeout" else KeyboardInterrupt):
                            ci.run_command([sys.executable, "-c", parent], directory, remaining)
                    self.assertLess(time.monotonic() - started, 5)
                    self.assertTrue(pidfile.exists())
                    pid = int(pidfile.read_text())
                    end = time.monotonic() + 2
                    while time.monotonic() < end:
                        status = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True)
                        if status.returncode or status.stdout.strip().startswith("Z"):
                            break
                        time.sleep(.02)
                    else:
                        self.fail("CI descendant survived cleanup")
                finally:
                    if pidfile.exists():
                        try:
                            os.kill(int(pidfile.read_text()), signal.SIGKILL)
                        except ProcessLookupError:
                            pass

    def test_full_checks_pass_and_push_only_candidate_without_force(self):
        self.assertEqual(self.invoke()["status"], "passed")
        pushes = [a for a in self.calls if a[:2] == ["git", "push"]]
        self.assertEqual(pushes, [["git", "push", "--no-mirror", "--no-follow-tags", "--recurse-submodules=no", "origin", f"{self.commit}:refs/heads/work"]])

    def test_wrong_branch_or_fork_rejected_before_push(self):
        for mode in ("branch", "fork", "base", "main"):
            with self.subTest(mode=mode):
                self.setUp()
                if mode == "branch": self.pr["head"]["ref"] = "other"
                if mode == "fork": self.pr["head"]["repo"]["full_name"] = "fork/repo"
                if mode == "base": self.pr["base"]["ref"] = "work"
                if mode == "main": self.branch = "main"
                with self.assertRaises(ValueError): self.invoke()
                self.assertFalse(any(a[:2] == ["git", "push"] for a in self.calls))

    def test_missing_checks_wait_until_deadline(self):
        self.checks.pop()
        initial = dt.datetime.now(dt.timezone.utc)
        with patch.object(ci.dt, "datetime", wraps=dt.datetime) as clock:
            clock.now.return_value = initial
            def expire(_): clock.now.return_value = self.until + dt.timedelta(seconds=1)
            with patch.object(ci.time, "sleep", side_effect=expire) as sleeper:
                with self.assertRaisesRegex(TimeoutError, "deadline reached"): self.invoke()
                sleeper.assert_called_once()

    def test_failed_check_stops(self):
        self.checks[0]["conclusion"] = "failure"
        with self.assertRaisesRegex(ValueError, "CI failed"): self.invoke()

    def test_head_changed_after_checks_stops(self):
        self.change_head = True
        with self.assertRaisesRegex(ValueError, "head changed"): self.invoke()

    def test_expired_deadline_runs_nothing(self):
        self.until = dt.datetime.now(dt.timezone.utc) - dt.timedelta(seconds=1)
        with self.assertRaises(TimeoutError): self.invoke()
        self.assertEqual(self.calls, [])

    def test_delayed_pr_projection_catches_up(self):
        self.head_sequence = ["b" * 40, "b" * 40, self.commit]
        with patch.object(ci.time, "sleep") as sleeper:
            self.assertEqual(self.invoke()["status"], "passed")
            sleeper.assert_called_once()

    def test_unrelated_head_during_startup_fails(self):
        self.head_sequence = ["b" * 40, "c" * 40]
        with self.assertRaisesRegex(ValueError, "head changed"):
            self.invoke()

    def test_unrelated_head_after_successful_checks_fails(self):
        self.head_sequence = [self.commit, self.commit, self.commit, "c" * 40]
        with self.assertRaisesRegex(ValueError, "head changed"):
            self.invoke()

    def test_unsafe_push_destinations_fail_before_push(self):
        for destination in ("git@github.com:other/repo.git",
                            "git@github.com:owner/repo.git\ngit@github.com:other/repo.git"):
            with self.subTest(destination=destination):
                self.setUp()
                self.push_url = destination
                with self.assertRaises(ValueError):
                    self.invoke()
                self.assertFalse(any(a[:2] == ["git", "push"] for a in self.calls))

    def test_stale_or_foreign_app_checks_do_not_pass(self):
        for mode in ("stale", "foreign"):
            with self.subTest(mode=mode):
                self.setUp()
                if mode == "stale":
                    self.checks[0]["head_sha"] = "b" * 40
                else:
                    self.checks[0]["app"]["slug"] = "other-app"
                initial = dt.datetime.now(dt.timezone.utc)
                with patch.object(ci.dt, "datetime", wraps=dt.datetime) as clock:
                    clock.now.return_value = initial
                    def expire(_): clock.now.return_value = self.until + dt.timedelta(seconds=1)
                    with patch.object(ci.time, "sleep", side_effect=expire):
                        with self.assertRaises(TimeoutError):
                            self.invoke()

    def test_latest_successful_rerun_overrides_previous_failure(self):
        old = dict(self.checks[0], id=-1, conclusion="failure")
        self.checks.append(old)
        self.assertEqual(self.invoke()["status"], "passed")

    def test_latest_failed_rerun_overrides_previous_success(self):
        latest = dict(self.checks[0], id=1000, conclusion="failure")
        self.checks.append(latest)
        with self.assertRaisesRegex(ValueError, "CI failed"):
            self.invoke()


if __name__ == "__main__": unittest.main()
