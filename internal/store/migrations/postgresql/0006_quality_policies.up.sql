-- M2 project quality-policy storage (P4, S4b).
--
-- Authority: docs/quality/quality-policy.md section 8 (semantic-versioned
-- publication with expected current version; same version with different
-- content is rejected) and the frozen control-plane.yaml
-- putProjectQualityPolicy (If-Match/If-None-Match + ETag). The company
-- baseline ships embedded with the binary (permissions.yaml precedent);
-- only the project overlay is stored, and only as the single current row
-- per project with compare-and-swap row versions. Deviations are
-- recorded in migrations/postgresql/README.md.

CREATE TABLE quality_policies (
    id            uuid PRIMARY KEY,
    project_id    uuid NOT NULL REFERENCES projects (id),
    scope         text NOT NULL DEFAULT 'project' CHECK (scope = 'project'),
    policy_id     text NOT NULL,
    semver        text NOT NULL,
    policy        jsonb NOT NULL,
    policy_digest text NOT NULL CHECK (policy_digest ~ '^sha256:[0-9a-f]{64}$'),
    row_version   bigint NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id)
);

-- The frozen evidence wire schema requires the attempt number (flaky
-- retry bookkeeping: initial failure and the one allowed retry both
-- persist); 0004 modeled the row without it.
ALTER TABLE evidence ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1);
