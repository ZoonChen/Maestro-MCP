-- M2 project quality-policy storage and wire-complete quality columns
-- (P4, S4b).
--
-- Authority: docs/quality/quality-policy.md section 8 (semantic-versioned
-- publication with expected current version) and the frozen
-- control-plane.yaml quality endpoints (putProjectQualityPolicy
-- If-Match/If-None-Match + ETag, Gate/Waiver ResourceVersion,
-- WaiverRequest.merge_request_iid). The company baseline ships embedded
-- with the binary (permissions.yaml precedent); only the project overlay
-- is stored. Deviations are recorded in migrations/postgresql/README.md.

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

-- The frozen evidence wire schema requires the outcome status (every
-- quality conclusion carries passed/failed/error/cancelled/skipped);
-- 0004 modeled the row without it. The attempt number carries the
-- flaky-retry bookkeeping (initial failure and the one allowed retry
-- both persist).
ALTER TABLE evidence ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1);
ALTER TABLE evidence ADD COLUMN status text NOT NULL DEFAULT 'passed'
    CHECK (status IN ('passed', 'failed', 'error', 'cancelled', 'skipped'));
ALTER TABLE evidence ADD COLUMN sensitivity text NOT NULL DEFAULT 'confidential'
    CHECK (sensitivity IN ('internal', 'confidential', 'restricted'));

-- ResourceVersion for the Gate and Waiver wire shapes: optimistic
-- concurrency for waiver approve/revoke (If-Match) and snapshot
-- re-evaluation. The waiver's merge request binding is part of the
-- frozen WaiverRequest/Waiver wire shape.
ALTER TABLE gate_snapshots ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version >= 1);
ALTER TABLE waivers ADD COLUMN merge_request_iid bigint NOT NULL CHECK (merge_request_iid >= 1);
ALTER TABLE waivers ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version >= 1);
