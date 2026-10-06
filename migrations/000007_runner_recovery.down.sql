-- Do not silently erase dispatch intent on downgrade.
ALTER TABLE runner_journal DROP COLUMN reconcile_until;
ALTER TABLE runner_journal DROP COLUMN reconcile_owner;
ALTER TABLE runner_journal DROP COLUMN runner_id;
ALTER TABLE runner_journal DROP CONSTRAINT runner_journal_status_check;
ALTER TABLE runner_journal ADD CONSTRAINT runner_journal_status_check CHECK (status IN ('RESERVED','SUCCEEDED','UNKNOWN','REJECTED'));
