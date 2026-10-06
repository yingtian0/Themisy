-- Execute once as the schema owner AFTER Runner-only migrations. Bind this
-- NOLOGIN role to a workload-specific LOGIN managed by the customer.
CREATE ROLE themisy_runner_runtime NOLOGIN;
GRANT USAGE ON SCHEMA themisy_runner TO themisy_runner_runtime;
GRANT SELECT ON ALL TABLES IN SCHEMA themisy_runner TO themisy_runner_runtime;
GRANT INSERT, UPDATE ON themisy_runner.runner_journal,
 themisy_runner.plan_revisions, themisy_runner.plan_manifests TO themisy_runner_runtime;
GRANT INSERT ON themisy_runner.audit_events TO themisy_runner_runtime;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA themisy_runner TO themisy_runner_runtime;
-- No CREATE, DELETE, TRUNCATE or audit UPDATE. Grant no Control Plane tables.
