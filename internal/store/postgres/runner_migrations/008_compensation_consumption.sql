ALTER TABLE runner_journal ADD COLUMN compensation_source_nonce text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX runner_compensation_once ON runner_journal(tenant_id,runner_group,compensation_source_nonce) WHERE compensation_source_nonce <> '';
