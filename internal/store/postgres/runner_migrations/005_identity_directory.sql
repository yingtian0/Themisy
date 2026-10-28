CREATE TABLE IF NOT EXISTS identity_directory_versions (
 tenant_id text NOT NULL,
 issuer text NOT NULL,
 revision bigint NOT NULL CHECK (revision > 0),
 bundle_hash text NOT NULL,
 PRIMARY KEY (tenant_id,issuer)
);
