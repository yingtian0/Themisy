DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM identity_directory_versions) THEN
  RAISE EXCEPTION 'refusing to discard identity directory replay protection';
 END IF;
END $$;
DROP TABLE identity_directory_versions;
