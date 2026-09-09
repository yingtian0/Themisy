ALTER TABLE action_grants
    ADD COLUMN result jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN leased_by text,
    ADD COLUMN delivery_token text,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN acknowledged_at timestamptz,
    ADD COLUMN completed_at timestamptz,
    ADD COLUMN outbox_id text,
    ADD COLUMN updated_at timestamptz;

UPDATE action_grants SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE action_grants ALTER COLUMN updated_at SET NOT NULL;

CREATE INDEX action_grants_dispatch_idx
    ON action_grants (tenant_id, runner_group, created_at)
    WHERE status IN ('PENDING', 'LEASED', 'ACKED');
