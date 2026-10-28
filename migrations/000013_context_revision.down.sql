DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM context_revision_fences) THEN
 RAISE EXCEPTION 'refusing to discard trusted Context revision fences';
 END IF;
END $$;
DROP TABLE context_revision_fences;
