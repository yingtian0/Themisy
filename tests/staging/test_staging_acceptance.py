import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts"))
import staging_acceptance as sa  # noqa: E402

CFG = {"expected_account_id": "111111111111", "region": "ap-northeast-1", "cluster": "stg-cluster",
       "service": "stg-api", "service_arn": "arn:aws:ecs:ap-northeast-1:111111111111:service/stg-cluster/stg-api",
       "denied_principals": ["arn:aws:iam::111111111111:role/ci"],
       "runner_role_arn": "arn:aws:iam::111111111111:role/runner",
       "runner_denied_resources": ["arn:aws:ecs:ap-northeast-1:111111111111:service/other/x"]}


def fake(decision, account="111111111111", active=True, fail=False):
    def run(*args):
        if fail:
            raise RuntimeError("boom")
        if args[0] == "sts":
            return {"Account": account}
        if args[0] == "ecs":
            return {"services": [{"status": "ACTIVE"}] if active else []}
        acts = args[args.index("--action-names") + 1:args.index("--resource-arns")]
        return {"EvaluationResults": [{"EvalActionName": x, "EvalDecision": decision(x)} for x in acts]}
    return run


class Test(unittest.TestCase):
    def run_cfg(self, fn, cmd, cfg=CFG):
        sa.aws = fn
        return sa.overall(sa.preflight(cfg) if cmd == "p" else sa.iam_denial(cfg))

    def test_preflight_pass_and_wrong_account(self):
        self.assertEqual(self.run_cfg(fake(lambda a: "implicitDeny"), "p"), sa.PASS)
        self.assertEqual(self.run_cfg(fake(lambda a: "", account="222222222222"), "p"), sa.FAIL)

    def test_production_name_refused(self):
        self.assertEqual(self.run_cfg(fake(lambda a: ""), "p", dict(CFG, service="prod-api")), sa.FAIL)

    def test_errors_are_inconclusive_never_pass(self):
        self.assertEqual(self.run_cfg(fake(lambda a: "", fail=True), "p"), sa.INCONCLUSIVE)
        self.assertEqual(self.run_cfg(fake(lambda a: "", fail=True), "i"), sa.INCONCLUSIVE)

    def test_denial_pass_fail(self):
        self.assertEqual(self.run_cfg(fake(lambda a: "implicitDeny"), "i"), sa.PASS)
        self.assertEqual(self.run_cfg(fake(lambda a: "allowed" if a == "ecs:UpdateService" else "implicitDeny"), "i"), sa.FAIL)

    def test_no_principals_inconclusive(self):
        self.assertEqual(self.run_cfg(fake(lambda a: "implicitDeny"), "i", dict(CFG, denied_principals=[])), sa.INCONCLUSIVE)

    def test_evidence_not_overwritten(self):
        with tempfile.TemporaryDirectory() as d:
            cfg = Path(d, "c.json"); cfg.write_text(json.dumps(CFG))
            ev = Path(d, "e.json"); ev.write_text("x")
            with self.assertRaises(SystemExit):
                sa.main(["preflight", str(cfg), "--evidence", str(ev)])


if __name__ == "__main__":
    unittest.main()
