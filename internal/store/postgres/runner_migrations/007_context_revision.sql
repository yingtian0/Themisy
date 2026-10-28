CREATE TABLE IF NOT EXISTS context_revision_fences (
 tenant_id text NOT NULL,
 issuer text NOT NULL,
 revision bigint NOT NULL CHECK(revision>0),
 context_hash text NOT NULL,
 PRIMARY KEY(tenant_id,issuer)
);
