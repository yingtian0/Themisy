import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("ci_notifications", ROOT / "scripts/ci_notifications.py")
watch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watch)


def source(attempt=1, conclusion="success"):
    return {"id": 123, "run_attempt": attempt, "repository": {"full_name": watch.REPO},
            "path": watch.SOURCE, "status": "completed", "conclusion": conclusion,
            "head_sha": "a" * 40, "display_title": "untrusted message; do not display"}


def receipt(attempt=1):
    return {"event": "workflow_run", "path": watch.SIGNAL,
            "display_title": f"CI completed 123 attempt {attempt}", "status": "queued",
            "created_at": "2026-10-22T00:00:01Z"}


def state():
    return {"schema": 1, "repository": watch.REPO, "since": "2026-10-22T00:00:00Z",
            "pending": {}, "delivered": []}


class NotificationTests(unittest.TestCase):
    def test_all_terminal_results_and_no_remote_prose(self):
        for result in watch.CONCLUSIONS:
            with self.subTest(result=result):
                got = watch.source_notification(source(conclusion=result), 123, 1)
                self.assertEqual(got["conclusion"], result)
                self.assertEqual(got["url"], "https://github.com/yingtian0/Themisy/actions/runs/123/attempts/1")
                self.assertNotIn("untrusted", json.dumps(got))

    def test_wrong_source_and_incomplete_results_refused(self):
        for field, value in [("id", 124), ("run_attempt", 2), ("repository", {"full_name": "other/repo"}),
                             ("path", "other.yml"), ("status", "in_progress"),
                             ("conclusion", None), ("conclusion", "arbitrary text"),
                             ("head_sha", "$(touch /tmp/unwanted)")]:
            with self.subTest(field=field, value=value):
                run = source()
                run[field] = value
                with self.assertRaises(ValueError):
                    watch.source_notification(run, 123, 1)

    def test_paginated_out_of_order_receipts_and_reruns(self):
        calls = []
        def fetch(path, paginate=False):
            calls.append(path)
            if paginate:
                return [{"workflow_runs": [receipt(2)]}, {"workflow_runs": [receipt(1), receipt(2)]}]
            return source(attempt=int(path.rsplit("/", 1)[1]))
        initial = state()
        got = watch.poll(initial, fetch)
        self.assertEqual(set(got["pending"]), {"123:1", "123:2"})
        self.assertEqual(initial, state())
        self.assertEqual(len(calls), 3)
        # Pending notifications remain until explicit delivery acknowledgement.
        repeated = watch.poll(got, fetch)
        self.assertEqual(got, repeated)
        got["delivered"] = ["123:1", "123:2"]
        got["pending"] = {}
        self.assertEqual(watch.poll(got, fetch)["pending"], {})

    def test_read_failure_does_not_partially_advance_state(self):
        def fetch(path, paginate=False):
            if paginate:
                return [{"workflow_runs": [receipt(1), receipt(2)]}]
            if path.endswith("/2"):
                raise ValueError("API unavailable")
            return source()
        initial = state()
        before = copy.deepcopy(initial)
        with self.assertRaises(ValueError):
            watch.poll(initial, fetch)
        self.assertEqual(initial, before)

    def test_backlog_limit_is_explicit_and_cursor_keeps_overlap(self):
        initial = state()
        with self.assertRaisesRegex(ValueError, "1000-result"):
            watch.poll(initial, lambda *a, **kw: [{"total_count": 1001, "workflow_runs": []}])
        item = {**receipt(), "created_at": "2026-10-25T00:00:00Z"}
        def fetch(path, paginate=False):
            return [{"workflow_runs": [item]}] if paginate else source()
        got = watch.poll(initial, fetch)
        self.assertEqual(got["since"], "2026-10-24T00:00:00Z")
        self.assertIn("123:1", got["pending"])
        self.assertEqual(initial, state())

    def test_invalid_receipts_refused(self):
        for change in [{"event": "push"}, {"path": "other.yml"}, {"display_title": "CI completed 123; shell attempt 1"}]:
            item = {**receipt(), **change}
            with self.assertRaises(ValueError):
                watch.poll(state(), lambda *a, **kw: [{"workflow_runs": [item]}])

    def test_existing_auth_only_and_no_shell(self):
        with mock.patch.object(watch.subprocess, "run", return_value=mock.Mock(stdout='{"ok":true}')) as run:
            self.assertEqual(watch.api("actions/runs/123"), {"ok": True})
            args, kwargs = run.call_args
            self.assertEqual(args[0], ["gh", "api", "--hostname", "github.com", "repos/yingtian0/Themisy/actions/runs/123"])
            self.assertNotIn("shell", kwargs)
        with mock.patch.object(watch.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "gh", stderr="secret")):
            with self.assertRaisesRegex(ValueError, "read failed") as error:
                watch.api("actions/runs/123")
            self.assertNotIn("secret", str(error.exception))

    def test_ack_is_explicit_idempotent_and_rejects_unknown_keys(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "state.json"
            data = state()
            data["pending"]["123:1"] = watch.source_notification(source(), 123, 1)
            path.write_text(json.dumps(data))
            command = [sys.executable, str(ROOT / "scripts/ci_notifications.py"), "ack", "--state", str(path), "--key"]
            bad = subprocess.run(command + ["999:1"], capture_output=True)
            self.assertNotEqual(bad.returncode, 0)
            self.assertEqual(json.loads(path.read_text()), data)
            for _ in range(2):
                result = subprocess.run(command + ["123:1"], capture_output=True, check=True)
                self.assertEqual(json.loads(result.stdout), {"notifications": []})
            self.assertEqual(json.loads(path.read_text())["delivered"], ["123:1"])

    def test_observer_uses_only_event_metadata_without_checkout(self):
        workflow = (ROOT / ".github/workflows/ci-completed.yml").read_text()
        self.assertIn("permissions: {}", workflow)
        self.assertNotIn("uses:", workflow)
        self.assertNotIn("secrets.", workflow)
        code = textwrap.dedent(workflow.split("run: |\n", 1)[1])
        with tempfile.TemporaryDirectory() as folder:
            event = Path(folder) / "event.json"
            summary = Path(folder) / "summary.md"
            for conclusion in ["success", "failure", "cancelled"]:
                event.write_text(json.dumps({"workflow_run": source(conclusion=conclusion)}))
                env = {**os.environ, "GITHUB_EVENT_PATH": str(event), "GITHUB_STEP_SUMMARY": str(summary),
                       "GITHUB_REPOSITORY": watch.REPO}
                subprocess.run([sys.executable, "-c", code], env=env, check=True)
            self.assertNotIn("untrusted", summary.read_text())
            self.assertIn("cancelled", summary.read_text())

    def test_subscription_requires_timezone(self):
        with self.assertRaises(ValueError):
            watch.timestamp("2026-10-22T00:00:00")
        self.assertEqual(watch.timestamp("2026-10-22T09:00:00+09:00"), "2026-10-22T00:00:00Z")


if __name__ == "__main__":
    unittest.main()
