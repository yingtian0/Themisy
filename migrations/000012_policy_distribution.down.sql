DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM policy_assignments) THEN
 RAISE EXCEPTION 'refusing to discard active Policy assignment history';
 END IF;
END $$;
DROP TABLE policy_acknowledgements;
DROP TABLE policy_assignments;
DROP TABLE scoped_policy_bundles;
