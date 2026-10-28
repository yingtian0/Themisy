CREATE TABLE scoped_policy_bundles (
 tenant_id text NOT NULL,
 bundle_hash text NOT NULL,
 issuer text NOT NULL,
 version text NOT NULL,
 payload jsonb NOT NULL,
 PRIMARY KEY(tenant_id,bundle_hash),
 UNIQUE(tenant_id,issuer,version)
);
CREATE TABLE policy_assignments (
 tenant_id text NOT NULL,
 runner_group text NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 bundle_hash text NOT NULL,
 previous_hash text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,runner_group),
 FOREIGN KEY(tenant_id,bundle_hash) REFERENCES scoped_policy_bundles(tenant_id,bundle_hash)
);
CREATE TABLE policy_acknowledgements (
 tenant_id text NOT NULL,
 runner_group text NOT NULL,
 runner_id text NOT NULL,
 revision bigint NOT NULL,
 bundle_hash text NOT NULL,
 observed_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,runner_group,runner_id,revision)
);
