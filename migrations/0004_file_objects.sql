-- idp-workspace schema v4 (ADR-0001 D24): a member file's content may live in an S3-compatible object store. A row then
-- names its object (files/<tenant>/<id>) and holds no content; rows written before keep theirs. Exactly one of the two.
ALTER TABLE member_files ALTER COLUMN content DROP NOT NULL;
ALTER TABLE member_files ADD COLUMN object_key text;
ALTER TABLE member_files DROP CONSTRAINT IF EXISTS member_files_check;
ALTER TABLE member_files ADD CONSTRAINT member_files_content CHECK (
  (object_key IS NULL AND content IS NOT NULL AND octet_length(content) = size)
  OR (content IS NULL AND object_key = 'files/' || tenant_id::text || '/' || id::text));
