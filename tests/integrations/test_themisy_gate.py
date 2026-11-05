import json
import stat
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))
import themisy_gate as g  # noqa: E402


def fake_ctl(d, responses):
    """A fake themisyctl that prints successive status bodies, then repeats the last."""
    d = Path(d)
    (d / "n").write_text("0")
    (d / "r.json").write_text(json.dumps(responses))
    script = d / "ctl"
    script.write_text(f"""#!/usr/bin/env python3
import json,sys
from pathlib import Path
d=Path({str(d)!r}); r=json.loads((d/'r.json').read_text()); n=int((d/'n').read_text()); (d/'n').write_text(str(n+1))
v=r[min(n,len(r)-1)]
if v=='ERR': sys.stderr.write('boom'); sys.exit(1)
print(json.dumps(v))
""")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    return str(script)


class Test(unittest.TestCase):
    def wait(self, responses, timeout=100):
        with tempfile.TemporaryDirectory() as d:
            ctl = fake_ctl(d, responses)
            t = [0]
            return g.wait("r1", ctl, timeout, 10, sleep=lambda s: t.__setitem__(0, t[0] + s), clock=lambda: t[0])[0]

    def test_succeeds_only_on_succeeded(self):
        self.assertEqual(self.wait([{"status": "RUNNING"}, {"status": "SUCCEEDED"}]), g.OK)

    def test_bad_terminal_states_fail(self):
        for s in ("FAILED", "BLOCKED", "ESCALATED", "CANCELLED"):
            self.assertEqual(self.wait([{"status": s}]), g.FAILED, s)

    def test_waiting_approval_times_out_as_pending_not_success(self):
        self.assertEqual(self.wait([{"status": "WAITING_APPROVAL"}], timeout=30), g.PENDING)

    def test_unknown_status_and_errors_raise(self):
        with self.assertRaises(RuntimeError):
            self.wait([{"status": "WEIRD"}])
        with self.assertRaises(RuntimeError):
            self.wait(["ERR"])
        with self.assertRaises(RuntimeError):
            self.wait([{"nostatus": 1}])

    def test_verify_plan_binds_hash_and_environment(self):
        run = {"status": "SUCCEEDED", "plan": {"hash": "h1"}, "environment": "staging"}
        self.assertTrue(g.verify_plan(run, "h1", "staging")[0])
        self.assertFalse(g.verify_plan(run, "h2", "staging")[0])
        self.assertFalse(g.verify_plan(run, "h1", "production")[0])
        self.assertFalse(g.verify_plan(dict(run, status="RUNNING"), "h1", None)[0])

    def test_main_maps_errors_to_nonzero(self):
        with tempfile.TemporaryDirectory() as d:
            ctl = fake_ctl(d, ["ERR"])
            self.assertEqual(g.main(["--themisyctl", ctl, "wait", "r1", "--timeout", "1"]), g.ERROR)


if __name__ == "__main__":
    unittest.main()
