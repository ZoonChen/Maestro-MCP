-- M2 instance/mapping resource versions (P4, S4a).
--
-- Authority: the frozen control-plane.yaml GitLabInstance and
-- GitLabProjectMapping wire shapes both carry ResourceVersion, and
-- putGitLabProjectMapping is if-match-or-if-none-match-required. 0004
-- modeled both tables without a version column; the optimistic
-- concurrency this endpoint family needs lands with the connector REST
-- slice. Deviations are recorded in migrations/postgresql/README.md.

ALTER TABLE gitlab_instances ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version >= 1);
ALTER TABLE gitlab_project_mappings ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version >= 1);

-- The wire GitLabProjectMapping carries an Identifier; 0004 keyed the
-- table by the composite external identity only.
ALTER TABLE gitlab_project_mappings ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX idx_gitlab_mappings_row_id ON gitlab_project_mappings (id);
-- One current mapping per project (putGitLabProjectMapping semantics).
CREATE UNIQUE INDEX idx_gitlab_mappings_project_unique ON gitlab_project_mappings (project_id);
