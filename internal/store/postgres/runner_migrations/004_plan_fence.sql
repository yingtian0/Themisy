ALTER TABLE runner_journal ADD COLUMN plan_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE runner_journal ADD COLUMN plan_hash text NOT NULL DEFAULT '';
