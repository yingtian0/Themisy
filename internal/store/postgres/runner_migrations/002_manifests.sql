CREATE TABLE plan_revisions (
 tenant_id text NOT NULL, runner_group text NOT NULL, run_id text NOT NULL,
 revision bigint NOT NULL CHECK(revision>0), plan_hash text NOT NULL,
 PRIMARY KEY(tenant_id,runner_group,run_id)
);
CREATE TABLE plan_manifests (
 tenant_id text NOT NULL, runner_group text NOT NULL, run_id text NOT NULL,
 step_id text NOT NULL, capability text NOT NULL, revision bigint NOT NULL,
 content_hash text NOT NULL, payload jsonb NOT NULL,
 PRIMARY KEY(tenant_id,runner_group,run_id,step_id,capability,revision)
);
