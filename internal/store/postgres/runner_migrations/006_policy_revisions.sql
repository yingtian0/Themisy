CREATE TABLE IF NOT EXISTS policy_revision_fences (
 tenant_id text NOT NULL,
 runner_group text NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 bundle_hash text NOT NULL,
 PRIMARY KEY(tenant_id,runner_group)
);
