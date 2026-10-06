ALTER TABLE runner_journal DROP CONSTRAINT runner_journal_status_check;
ALTER TABLE runner_journal ADD CONSTRAINT runner_journal_status_check CHECK (status IN ('RECEIVED','RESERVED','DISPATCHED','SUCCEEDED','FAILED','UNKNOWN','REJECTED'));
ALTER TABLE runner_journal ADD COLUMN runner_id text NOT NULL DEFAULT '';
ALTER TABLE runner_journal ADD COLUMN reconcile_owner text NOT NULL DEFAULT '';
ALTER TABLE runner_journal ADD COLUMN reconcile_until timestamptz;
