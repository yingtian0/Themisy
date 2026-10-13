import datetime as dt
import importlib.util
import json
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
        owner = self
        class FakeProcess:
            def __init__(self, args, **kwargs):
                self.args = args
                self.returncode = 0
            def communicate(self, timeout=None):
                return owner.command(self.args, timeout=timeout), ""
        with patch.object(ci.subprocess, "Popen", FakeProcess):
            return ci.verify_ci("/unused", 1, self.commit, self.until)

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
