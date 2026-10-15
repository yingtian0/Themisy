import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


MODULE = Path(__file__).resolve().parents[2] / "scripts" / "agent_cycle.py"
spec = importlib.util.spec_from_file_location("agent_cycle_packet", MODULE)
cycle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cycle)


class PacketTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.git("init", "-q")
        self.git("config", "user.name", "Packet Test")
        self.git("config", "user.email", "packet@example.invalid")
        (self.root / "code.txt").write_text("baseline\n")
        self.git("add", "code.txt")
        self.git("commit", "-qm", "baseline")
        self.commit = self.git("rev-parse", "HEAD").strip()
        self.state = self.root / "local-state"
        self.state.mkdir()
        for name in ("review", "format", "unit", "race", "lint"):
            (self.state / f"{name}.log").write_text(f"{name}: recorded evidence\n")
        self.packet = {
            "task_id": "test-packet", "objective": "Verify one bounded change",
            "implementer": "implementation-agent", "candidate_commit": self.commit,
            "acceptance": ["Required behavior covered by regression test"],
            "product_decision": {"basis": "accepted_issue", "reference": "issue-8"},
            "review": {"verdict": "pass", "commit": self.commit,
                       "reviewer": "review-agent", "log": "review.log"},
            "required_checks": [],
            "checks": [{"name": name, "command": f"make {name}",
                        "commit": self.commit, "exit_code": 0, "log": f"{name}.log"}
                       for name in ("format", "unit", "race", "lint")],
            "ci": {"status": "not_run", "commit": self.commit},
        }

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.root), *args], text=True)

    def validate(self, packet=None):
        (self.state / "packet.json").write_text(json.dumps(self.packet if packet is None else packet))
        return cycle.validate_packet(self.root, self.state, "packet.json")

    def test_clean_candidate_accepts_local_evidence(self):
        result = self.validate()
        self.assertEqual(result["candidate_commit"], self.commit)
        self.assertEqual(result["verification"], "local_only")
        self.assertEqual(len(result["packet_sha256"]), 64)

    def test_previous_commit_cannot_authorize_new_head(self):
        (self.root / "code.txt").write_text("new implementation\n")
        self.git("add", "code.txt")
        self.git("commit", "-qm", "new candidate")
        with self.assertRaisesRegex(ValueError, "current HEAD"):
            self.validate()

    def test_stale_review_or_check_rejected(self):
        for subject in ("review", "check"):
            with self.subTest(subject=subject):
                packet = copy.deepcopy(self.packet)
                record = packet["review"] if subject == "review" else packet["checks"][0]
                record["commit"] = "0" * 40
                with self.assertRaises(ValueError):
                    self.validate(packet)

    def test_missing_standard_or_task_specific_check_rejected(self):
        for subject in ("standard", "task"):
            with self.subTest(subject=subject):
                packet = copy.deepcopy(self.packet)
                if subject == "standard":
                    packet["checks"].pop()
                else:
                    packet["required_checks"] = ["postgres-recovery"]
                with self.assertRaisesRegex(ValueError, "required checks missing"):
                    self.validate(packet)

    def test_self_review_rejected(self):
        self.packet["review"]["reviewer"] = self.packet["implementer"]
        with self.assertRaisesRegex(ValueError, "independent"):
            self.validate()

    def test_identity_and_command_fields_are_typed_and_normalized(self):
        for reviewer in (" " + self.packet["implementer"] + " ", ["other"], True, " "):
            packet = copy.deepcopy(self.packet)
            packet["review"]["reviewer"] = reviewer
            with self.subTest(reviewer=reviewer), self.assertRaises(ValueError):
                self.validate(packet)
        for command in (True, ["make", "test"], " "):
            packet = copy.deepcopy(self.packet)
            packet["checks"][0]["command"] = command
            with self.subTest(command=command), self.assertRaises(ValueError):
                self.validate(packet)
        packet = copy.deepcopy(self.packet)
        packet["product_decision"] = {"basis": "independent_review", "reference": "proposal", "proposer": "same", "reviewer": " same "}
        with self.assertRaises(ValueError):
            self.validate(packet)

    def test_product_self_approval_rejected(self):
        self.packet["product_decision"] = {
            "basis": "independent_review", "reference": "proposal", "proposer": "same", "reviewer": "same"}
        with self.assertRaisesRegex(ValueError, "independent"):
            self.validate()

    def test_empty_review_and_check_logs_rejected(self):
        for name in ("review", "unit"):
            with self.subTest(name=name):
                path = self.state / f"{name}.log"
                original = path.read_text()
                path.write_text("")
                with self.assertRaisesRegex(ValueError, "empty evidence"):
                    self.validate()
                path.write_text(original)

    def test_invalid_ci_rejected(self):
        for ci in ({"status": "failed", "commit": self.commit},
                   {"status": "pending", "commit": self.commit},
                   {"status": "passed", "commit": "0" * 40, "url": "https://example.invalid/run"},
                   {"status": "passed", "commit": self.commit}):
            with self.subTest(ci=ci):
                packet = copy.deepcopy(self.packet)
                packet["ci"] = ci
                with self.assertRaises(ValueError):
                    self.validate(packet)

    def test_failed_and_boolean_exit_codes_rejected(self):
        for code in (1, False):
            with self.subTest(code=code):
                packet = copy.deepcopy(self.packet)
                packet["checks"][0]["exit_code"] = code
                with self.assertRaises(ValueError):
                    self.validate(packet)

    def test_packet_path_escape_rejected(self):
        with self.assertRaisesRegex(ValueError, "inside state-dir"):
            cycle.validate_packet(self.root, self.state, "../code.txt")

    def test_evidence_path_escape_and_symlink_rejected(self):
        # Isolate evidence containment from the separate untracked-file gate.
        (self.root / ".git" / "info" / "exclude").write_text("/local-state/\n")
        (self.state / "outside.log").symlink_to(self.root / "code.txt")
        for log in ("../code.txt", "outside.log"):
            with self.subTest(log=log):
                packet = copy.deepcopy(self.packet)
                packet["review"]["log"] = log
                with self.assertRaisesRegex(ValueError, "inside state-dir"):
                    self.validate(packet)

    def test_dirty_tracked_code_rejected_staged_or_unstaged(self):
        (self.root / "code.txt").write_text("unreviewed change\n")
        with self.assertRaisesRegex(ValueError, "uncommitted tracked"):
            self.validate()
        self.git("add", "code.txt")
        with self.assertRaisesRegex(ValueError, "uncommitted tracked"):
            self.validate()

    def test_untracked_code_outside_state_rejected(self):
        (self.root / "unreviewed.py").write_text("print('not reviewed')\n")
        with self.assertRaisesRegex(ValueError, "untracked implementation"):
            self.validate()


if __name__ == "__main__":
    unittest.main()
