CREATE TABLE runner_journal (
 tenant_id text NOT NULL, runner_group text NOT NULL, nonce text NOT NULL,
 grant_id text NOT NULL, run_id text NOT NULL, step_id text NOT NULL,
 idempotency_key text NOT NULL, request_hash text NOT NULL,
 target jsonb NOT NULL, action jsonb NOT NULL,
 status text NOT NULL CHECK(status IN ('RECEIVED','RESERVED','DISPATCHED','SUCCEEDED','FAILED','UNKNOWN','REJECTED')),
 result jsonb NOT NULL DEFAULT '{}', state_version bigint NOT NULL CHECK(state_version>0),
 created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 runner_id text NOT NULL, reconcile_owner text NOT NULL DEFAULT '', reconcile_until timestamptz,
 PRIMARY KEY(tenant_id,runner_group,nonce), UNIQUE(tenant_id,runner_group,idempotency_key)
);
CREATE INDEX runner_pending ON runner_journal(tenant_id,runner_group,status,updated_at);
CREATE TABLE audit_events (
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, id text NOT NULL UNIQUE,
 correlation_id text NOT NULL, actor_type text NOT NULL, actor_id text NOT NULL,
 delegated_by text, action text NOT NULL, resource_type text NOT NULL, resource_id text NOT NULL,
 result text NOT NULL, details jsonb NOT NULL, occurred_at timestamptz NOT NULL
);
CREATE FUNCTION reject_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit_events is append-only'; END $$;
CREATE TRIGGER audit_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_events FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_mutation();
