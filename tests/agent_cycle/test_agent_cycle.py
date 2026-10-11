import argparse
import datetime as dt
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import signal
import sys
import tempfile
import time
import unittest
from unittest import mock

MODULE = Path(__file__).resolve().parents[2] / 'scripts' / 'agent_cycle.py'
spec = importlib.util.spec_from_file_location('agent_cycle', MODULE)
cycle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cycle)


class CycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True)
        self.state = self.root / 'local-state'
        self.fake = self.root / 'fake-codex'
        self.args = argparse.Namespace(worktree=self.root, state_dir=self.state,
            until=dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=5),
            cycle_seconds=10, wrap_up_seconds=0, codex=str(self.fake), resume_interrupted=False, task_file=None, max_cycles=0)

    def install(self, body):
        self.fake.write_text('#!/usr/bin/env python3\nimport sys,json,time\nfrom pathlib import Path\n' + body)
        self.fake.chmod(0o755)

    def state_value(self):
        return json.loads((self.state / 'state.json').read_text())

    def wait_until(self, predicate, timeout=5):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if predicate():
                return
            time.sleep(0.02)
        self.fail('condition did not become true before timeout')

    def process_running(self, pid):
        # A killed orphan can briefly remain a zombie until init reaps it.
        result = subprocess.run(['ps', '-o', 'stat=', '-p', str(pid)], capture_output=True, text=True)
        return result.returncode == 0 and bool(result.stdout.strip()) and not result.stdout.strip().startswith('Z')

    def kill_group(self, pgid):
        try:
            os.killpg(pgid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def install_process_tree(self):
        self.child_pid = self.root / 'child.pid'
        child = ('import os,signal,time; from pathlib import Path; '
                 'signal.signal(signal.SIGTERM, signal.SIG_IGN); '
                 f'Path({str(self.child_pid)!r}).write_text(str(os.getpid())); '
                 'time.sleep(30)')
        self.install(f'''import subprocess
sys.stdin.read()
subprocess.Popen([sys.executable, '-c', {child!r}])
time.sleep(30)
''')

    def start_supervisor(self):
        process = subprocess.Popen([sys.executable, str(MODULE), '--worktree', str(self.root),
            '--state-dir', str(self.state), '--until', self.args.until.isoformat(),
            '--wrap-up-seconds', '0', '--cycle-seconds', str(self.args.cycle_seconds),
            '--codex', str(self.fake)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        def cleanup():
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
            if (self.state / 'state.json').exists():
                pgid = self.state_value().get('pgid')
                if pgid:
                    self.kill_group(pgid)
        self.addCleanup(cleanup)
        return process

    def test_sigterm_supervisor_stops_worker_and_descendant(self):
        self.install_process_tree()
        supervisor = self.start_supervisor()
        self.wait_until(self.child_pid.exists)
        child = int(self.child_pid.read_text())
        group = self.state_value()['pgid']
        self.addCleanup(self.kill_group, group)
        supervisor.send_signal(signal.SIGTERM)
        self.assertEqual(supervisor.wait(timeout=5), 130)
        self.wait_until(lambda: not self.process_running(child))
        self.assertFalse(self.process_running(group))
        self.assertEqual(self.state_value()['status'], 'interrupted')

    def test_stop_process_kills_term_resistant_descendant_after_leader_exits(self):
        self.install_process_tree()
        leader = subprocess.Popen([str(self.fake)], stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        self.addCleanup(leader.wait, timeout=5)
        self.addCleanup(self.kill_group, leader.pid)
        self.wait_until(self.child_pid.exists)
        child = int(self.child_pid.read_text())
        cycle.stop_process(leader)
        self.assertIsNotNone(leader.returncode)
        self.wait_until(lambda: not self.process_running(child))

    def test_stale_running_group_refuses_resume_even_when_requested(self):
        worker = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'], start_new_session=True)
        self.addCleanup(worker.wait, timeout=5)
        self.addCleanup(self.kill_group, worker.pid)
        self.state.mkdir()
        original = dict(cycles=[], status='running', active_cycle=1,
                        worktree=str(self.root), pgid=worker.pid)
        cycle.save(self.state / 'state.json', original)
        self.args.resume_interrupted = True
        with self.assertRaisesRegex(ValueError, 'previous process group still exists'):
            cycle.run(self.args)
        self.assertEqual(self.state_value(), original)
        self.assertIsNone(worker.poll())

    def test_real_deadline_stops_worker_tree(self):
        self.install_process_tree()
        self.args.until = dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=1)
        started = time.monotonic()
        supervisor = self.start_supervisor()
        self.wait_until(self.child_pid.exists)
        child = int(self.child_pid.read_text())
        group = self.state_value()['pgid']
        self.addCleanup(self.kill_group, group)
        self.assertEqual(supervisor.wait(timeout=5), 2)
        self.assertLess(time.monotonic() - started, 4)
        self.wait_until(lambda: not self.process_running(child))
        self.assertFalse(self.process_running(group))
        self.assertEqual(self.state_value()['status'], 'timeout')

    def test_successful_manager_exit_cleans_remaining_descendants(self):
        self.install_process_tree()
        body = self.fake.read_text().replace('time.sleep(30)\n', f'''while not Path({str(self.child_pid)!r}).exists():
    time.sleep(0.01)
Path(sys.argv[sys.argv.index('--output-last-message')+1]).write_text(json.dumps(dict(status='idle',packet='',summary='done',next_task='',evidence=[])))
''')
        self.fake.write_text(body)
        self.assertEqual(cycle.run(self.args), 0)
        child = int(self.child_pid.read_text())
        self.wait_until(lambda: not self.process_running(child))
        self.assertEqual(self.state_value()['status'], 'idle')
        self.assertNotIn('pgid', self.state_value())

    def test_checkpoint_failure_after_spawn_stops_worker(self):
        self.install('sys.stdin.read()\ntime.sleep(30)\n')
        real_save = cycle.save
        spawned = []
        def fail_once(path, value):
            if value.get('pgid') and not spawned:
                spawned.append(value['pgid'])
                raise OSError('checkpoint disk failure')
            return real_save(path, value)
        with mock.patch.object(cycle, 'save', side_effect=fail_once):
            with self.assertRaisesRegex(OSError, 'checkpoint disk failure'):
                cycle.run(self.args)
        self.assertEqual(len(spawned), 1)
        self.addCleanup(self.kill_group, spawned[0])
        self.assertFalse(self.process_running(spawned[0]))
        self.assertEqual(self.state_value()['status'], 'interrupted')

    def test_deadline_crossed_during_setup_does_not_spawn(self):
        self.install('raise RuntimeError("must not run")\n')
        self.args.until = dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=0.05)
        real_prompt = cycle.prompt
        def slow_prompt(*args):
            time.sleep(0.1)
            return real_prompt(*args)
        with mock.patch.object(cycle, 'prompt', side_effect=slow_prompt):
            self.assertEqual(cycle.run(self.args), 0)
        self.assertEqual(self.state_value()['status'], 'deadline')
        self.assertNotIn('pgid', self.state_value())

    def test_timezone_is_required(self):
        with self.assertRaises(argparse.ArgumentTypeError):
            cycle.deadline('2026-10-12T03:00:00')
        self.assertEqual(cycle.deadline('2026-10-12T03:00:00+09:00').hour, 3)

    def test_deadline_does_not_start_process(self):
        self.args.until = dt.datetime.now(dt.timezone.utc) - dt.timedelta(seconds=1)
        self.assertEqual(cycle.run(self.args), 0)
        self.assertEqual(self.state_value()['status'], 'deadline')

    def test_progress_then_idle_checkpoints_two_sessions(self):
        self.install('''sys.stdin.read()
p=Path(sys.argv[sys.argv.index('--output-last-message')+1])
status='progress' if p.name=='cycle-0001.json' else 'idle'
p.write_text(json.dumps(dict(status=status,packet='packet.json' if status=='progress' else '',summary='checked',next_task='',evidence=['test log'])))
''')
        with mock.patch.object(cycle, 'validate_packet', return_value={'verification': 'local_only'}):
            self.assertEqual(cycle.run(self.args), 0)
        state = self.state_value()
        self.assertEqual(state['status'], 'idle')
        self.assertEqual(len(state['cycles']), 2)

    def test_explicit_task_and_cycle_cap(self):
        task = self.root / 'authorized-task.txt'
        task.write_text('ONLY_THIS_BOUNDED_TASK')
        self.args.task_file = task
        self.args.max_cycles = 1
        self.install("""assert 'ONLY_THIS_BOUNDED_TASK' in sys.stdin.read()
p=Path(sys.argv[sys.argv.index('--output-last-message')+1])
p.write_text(json.dumps(dict(status='progress',packet='packet.json',summary='checked',next_task='next',evidence=[])))
""")
        with mock.patch.object(cycle, 'validate_packet', return_value={'verification': 'local_only'}):
            self.assertEqual(cycle.run(self.args), 0)
        self.assertEqual(self.state_value()['status'], 'cycle_limit')
        self.assertEqual(len(self.state_value()['cycles']), 1)

    def test_quota_or_other_failure_stops_without_retry(self):
        self.install('sys.stdin.read()\nprint("usage limit",file=sys.stderr)\nsys.exit(1)\n')
        self.assertEqual(cycle.run(self.args), 2)
        self.assertEqual(self.state_value()['status'], 'execution_failed')
        self.assertEqual(len(list(self.state.glob('*.stderr'))), 1)

    def test_invalid_result_stops(self):
        self.install('''sys.stdin.read()
Path(sys.argv[sys.argv.index('--output-last-message')+1]).write_text('{}')
''')
        self.assertEqual(cycle.run(self.args), 2)
        self.assertEqual(self.state_value()['status'], 'invalid_result')

    def test_timeout_stops(self):
        self.install('sys.stdin.read()\ntime.sleep(20)\n')
        self.args.cycle_seconds = 0.1
        self.assertEqual(cycle.run(self.args), 2)
        self.assertEqual(self.state_value()['status'], 'timeout')

    def test_repository_lock_covers_different_state_directory(self):
        with (self.root / '.git' / 'themisy-agent-cycle.lock').open('w') as lock:
            cycle.fcntl.flock(lock, cycle.fcntl.LOCK_EX | cycle.fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'another agent-cycle'):
                cycle.run(self.args)

    def test_interrupted_cycle_is_not_overwritten(self):
        self.state.mkdir()
        cycle.save(self.state / 'state.json', dict(cycles=[], status='running', active_cycle=1, worktree=str(self.root)))
        self.install('''sys.stdin.read()
Path(sys.argv[sys.argv.index('--output-last-message')+1]).write_text(json.dumps(dict(status='blocked',packet='',summary='review interrupted work',next_task='',evidence=[])))
''')
        with self.assertRaisesRegex(ValueError, 'interrupted cycle'):
            cycle.run(self.args)
        self.args.resume_interrupted = True
        self.assertEqual(cycle.run(self.args), 0)
        state = self.state_value()
        self.assertEqual(state['interrupted_cycle'], 1)
        self.assertEqual(state['cycles'][0]['index'], 2)


if __name__ == '__main__':
    unittest.main()
