DROP INDEX IF EXISTS action_grants_dispatch_idx;

ALTER TABLE action_grants
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS outbox_id,
    DROP COLUMN IF EXISTS completed_at,
    DROP COLUMN IF EXISTS acknowledged_at,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS delivery_token,
    DROP COLUMN IF EXISTS leased_by,
    DROP COLUMN IF EXISTS result;
