DROP INDEX IF EXISTS idx_gitlab_mappings_project_unique;
DROP INDEX IF EXISTS idx_gitlab_mappings_row_id;
ALTER TABLE gitlab_project_mappings DROP COLUMN IF EXISTS id;
ALTER TABLE gitlab_project_mappings DROP COLUMN IF EXISTS version;
ALTER TABLE gitlab_instances DROP COLUMN IF EXISTS version;
