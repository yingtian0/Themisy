DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM runner_journal WHERE compensation_source_nonce <> '') THEN
  RAISE EXCEPTION 'cannot discard populated compensation consumption fence';
 END IF;
END $$;
DROP INDEX runner_compensation_once;
ALTER TABLE runner_journal DROP COLUMN compensation_source_nonce;
