-- W4.5 Work Graph model + asset ledger (session J2a; ADR-009 approved
-- 2026-09-10, authority: docs/technical/work-graph-model.md and the
-- ARTIFACT-STANDARDS ledger semantics promoted through the ADR-009 review).
--
-- Expand step one only: nine NEW tables, zero changes to any existing
-- table or column (shadow composition / dual-read / write cutover are a
-- later contract ritual). Numbering: J1 owns 0016 (functional_principals);
-- first merger keeps its number, deviation recorded in the README.
--
--   work_plans / plan_revisions / work_nodes / work_node_revisions:
--     the versioned typed work graph. contains = work_nodes adjacency
--     (parent_node_id), requires = work_dependencies, lineage =
--     node_lineage. Sealed plan revisions and all node revisions are
--     tamper-proof at the trigger level, not just application discipline.
--
--   assets / asset_gate_bindings: the governed artifact ledger. Status
--     lifecycle draft->reviewed->approved->superseded, single-successor
--     supersede chains and per-version content immutability are machine
--     checks carried by CHECKs, partial unique indexes and triggers; the
--     application layer re-validates the same rules for stable errors.

-- ---------------------------------------------------------------------------
-- Work graph core
-- ---------------------------------------------------------------------------

CREATE TABLE work_plans (
    id           uuid PRIMARY KEY,
    project_id   uuid NOT NULL REFERENCES projects (id),
    title        text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    human_code   text NOT NULL CHECK (human_code ~ '^MST-WP-[0-9]{5}$'),
    root_node_id uuid,
    graph_version bigint NOT NULL DEFAULT 1 CHECK (graph_version > 0),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'proposed', 'sealed', 'executing', 'aggregating',
        'satisfied', 'failed', 'needs_human', 'replanned', 'cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, human_code)
);

CREATE INDEX idx_work_plans_project ON work_plans (project_id);

CREATE TABLE work_nodes (
    id             uuid PRIMARY KEY,
    plan_id        uuid NOT NULL REFERENCES work_plans (id),
    parent_node_id uuid,
    node_type      text NOT NULL CHECK (node_type IN ('work_package', 'work_item', 'gate')),
    slot_key       text NOT NULL CHECK (slot_key ~ '^[a-z][a-z0-9.]{0,63}$'),
    human_code     text NOT NULL CHECK (human_code ~ '^MST-(WP|WI)-[0-9]{5}$'),
    status         text NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'queued', 'leased', 'executing', 'validating',
        'ready_for_human_merge', 'done', 'blocked', 'cancelling',
        'cancelled', 'failed', 'needs_human', 'aggregating', 'satisfied')),
    node_version   bigint NOT NULL DEFAULT 1 CHECK (node_version > 0),
    depth          integer NOT NULL CHECK (depth >= 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plan_id, human_code),
    UNIQUE (id, plan_id),
    -- slot_key unique within (plan, parent, slot); NULLS NOT DISTINCT makes
    -- the NULL parent (root) a single distinct value so at most one root
    -- row per slot exists (WGM-INV-001).
    UNIQUE NULLS NOT DISTINCT (plan_id, parent_node_id, slot_key)
);

CREATE UNIQUE INDEX uniq_work_nodes_root ON work_nodes (plan_id) WHERE parent_node_id IS NULL;
CREATE INDEX idx_work_nodes_parent ON work_nodes (parent_node_id);

-- Structural identity of a node is immutable; only status/version
-- bookkeeping may change (WGM-INV-002).
CREATE OR REPLACE FUNCTION maestro_work_node_structure_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.plan_id IS DISTINCT FROM OLD.plan_id
       OR NEW.parent_node_id IS DISTINCT FROM OLD.parent_node_id
       OR NEW.node_type IS DISTINCT FROM OLD.node_type
       OR NEW.slot_key IS DISTINCT FROM OLD.slot_key
       OR NEW.human_code IS DISTINCT FROM OLD.human_code
       OR NEW.depth IS DISTINCT FROM OLD.depth THEN
        RAISE EXCEPTION 'WORK_NODE_STRUCTURE_IMMUTABLE: %', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_work_nodes_structure_immutable
    BEFORE UPDATE ON work_nodes
    FOR EACH ROW EXECUTE FUNCTION maestro_work_node_structure_immutable();

CREATE TABLE plan_revisions (
    id            uuid PRIMARY KEY,
    plan_id       uuid NOT NULL REFERENCES work_plans (id),
    revision_no   integer NOT NULL CHECK (revision_no > 0),
    status        text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'sealed')),
    spec_digest   text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    node_manifest jsonb NOT NULL DEFAULT '[]'::jsonb,
    sealed_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plan_id, revision_no)
);

-- A sealed plan revision rejects any UPDATE/DELETE at the trigger level
-- (WGM-INV-08); the draft->sealed transition itself is the guarded UPDATE.
CREATE OR REPLACE FUNCTION maestro_plan_revision_sealed_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'PLAN_REVISION_SEALED_IMMUTABLE: %', OLD.id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_plan_revisions_sealed_immutable
    BEFORE UPDATE OR DELETE ON plan_revisions
    FOR EACH ROW WHEN (OLD.status = 'sealed')
    EXECUTE FUNCTION maestro_plan_revision_sealed_immutable();

CREATE TABLE work_node_revisions (
    id               uuid PRIMARY KEY,
    node_id          uuid NOT NULL REFERENCES work_nodes (id),
    plan_revision_id uuid NOT NULL REFERENCES plan_revisions (id),
    spec_digest      text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    spec             jsonb NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, plan_revision_id)
);

-- Node revisions are append-only facts, full stop (WGM-INV-008).
CREATE TRIGGER trg_work_node_revisions_immutable
    BEFORE UPDATE OR DELETE ON work_node_revisions
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('WORK_NODE_REVISION');

CREATE TABLE work_dependencies (
    id           uuid PRIMARY KEY,
    plan_id      uuid NOT NULL REFERENCES work_plans (id),
    from_node_id uuid NOT NULL,
    to_node_id   uuid NOT NULL,
    requirement  text NOT NULL DEFAULT 'required' CHECK (requirement IN ('required', 'optional')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (from_node_id, to_node_id),
    CHECK (from_node_id <> to_node_id),
    -- Composite keys pin every requires edge inside one plan, so edges can
    -- never cross projects transitively (WGM-INV-002).
    FOREIGN KEY (from_node_id, plan_id) REFERENCES work_nodes (id, plan_id),
    FOREIGN KEY (to_node_id, plan_id) REFERENCES work_nodes (id, plan_id)
);

CREATE INDEX idx_work_dependencies_from ON work_dependencies (from_node_id);
CREATE INDEX idx_work_dependencies_to ON work_dependencies (to_node_id);

CREATE TABLE node_lineage (
    id                  uuid PRIMARY KEY,
    plan_id             uuid NOT NULL REFERENCES work_plans (id),
    node_id             uuid NOT NULL,
    predecessor_node_id uuid NOT NULL,
    kind                text NOT NULL CHECK (kind IN ('followup_of', 'replacement_of')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, predecessor_node_id, kind),
    CHECK (node_id <> predecessor_node_id),
    FOREIGN KEY (node_id, plan_id) REFERENCES work_nodes (id, plan_id),
    FOREIGN KEY (predecessor_node_id, plan_id) REFERENCES work_nodes (id, plan_id)
);

CREATE INDEX idx_node_lineage_node ON node_lineage (node_id);

-- The plan root is set exactly once and never re-pointed (WGM-INV-002).
CREATE OR REPLACE FUNCTION maestro_work_plan_root_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.root_node_id IS NOT NULL AND NEW.root_node_id IS DISTINCT FROM OLD.root_node_id THEN
        RAISE EXCEPTION 'WORK_PLAN_ROOT_IMMUTABLE: %', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_work_plans_root_immutable
    BEFORE UPDATE ON work_plans
    FOR EACH ROW EXECUTE FUNCTION maestro_work_plan_root_immutable();

ALTER TABLE work_plans
    ADD CONSTRAINT fk_work_plans_root FOREIGN KEY (root_node_id) REFERENCES work_nodes (id);

-- ---------------------------------------------------------------------------
-- Asset ledger
-- ---------------------------------------------------------------------------

CREATE TABLE assets (
    asset_id        text NOT NULL CHECK (asset_id ~ '^ART-[a-z-]+-[0-9]{3,}$'),
    version         integer NOT NULL CHECK (version > 0),
    project_id      uuid NOT NULL REFERENCES projects (id),
    asset_type      text NOT NULL CHECK (asset_type IN (
        'blueprint', 'prd', 'research', 'bom', 'hld', 'detailed-design',
        'test-plan', 'test-report', 'sec-review', 'ops-runbook',
        'deploy-plan', 'release-note', 'incident', 'retrospective',
        'legacy-intake')),
    title           text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    status          text NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'reviewed', 'approved', 'superseded')),
    owner_principal text NOT NULL,
    reviewers       jsonb NOT NULL DEFAULT '[]'::jsonb,
    sensitivity     text NOT NULL CHECK (sensitivity IN ('public', 'internal', 'confidential')),
    supersedes_ref  text CHECK (supersedes_ref ~ '^ART-[a-z-]+-[0-9]{3,}@[0-9]+$'),
    source_digest   text NOT NULL CHECK (source_digest ~ '^sha256:[0-9a-f]{64}$'),
    locked_gate     text,
    content_ref     text,
    summary         jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    reviewed_at     timestamptz,
    approved_at     timestamptz,
    superseded_at   timestamptz,
    PRIMARY KEY (asset_id, version),
    -- Lifecycle timestamps must agree with status (WGM-INV-013).
    CHECK (status = 'draft' OR reviewed_at IS NOT NULL),
    CHECK (status NOT IN ('approved', 'superseded') OR (reviewed_at IS NOT NULL AND approved_at IS NOT NULL)),
    CHECK (status <> 'superseded' OR superseded_at IS NOT NULL),
    CHECK (approved_at IS NULL OR approved_at >= created_at),
    CHECK (superseded_at IS NULL OR superseded_at >= created_at),
    -- Confidential intake is pointer-only by construction (content_ref may
    -- exist, but the ledger never carries content; application layer
    -- refuses to register confidential content blobs).
    CHECK (locked_gate IS NULL OR char_length(locked_gate) BETWEEN 1 AND 128)
);

-- A superseded version has exactly one successor (WGM-INV-014).
CREATE UNIQUE INDEX uniq_assets_supersedes ON assets (supersedes_ref) WHERE supersedes_ref IS NOT NULL;
CREATE INDEX idx_assets_project ON assets (project_id, asset_type);
CREATE INDEX idx_assets_status ON assets (status);

-- Per-version content fields never change; only lifecycle stamps move
-- (WGM-INV-013: a revision is a NEW version row).
CREATE OR REPLACE FUNCTION maestro_asset_content_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.asset_id IS DISTINCT FROM OLD.asset_id
       OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.asset_type IS DISTINCT FROM OLD.asset_type
       OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.owner_principal IS DISTINCT FROM OLD.owner_principal
       OR NEW.sensitivity IS DISTINCT FROM OLD.sensitivity
       OR NEW.supersedes_ref IS DISTINCT FROM OLD.supersedes_ref
       OR NEW.source_digest IS DISTINCT FROM OLD.source_digest
       OR NEW.locked_gate IS DISTINCT FROM OLD.locked_gate
       OR NEW.content_ref IS DISTINCT FROM OLD.content_ref
       OR NEW.summary IS DISTINCT FROM OLD.summary THEN
        RAISE EXCEPTION 'ASSET_VERSION_IMMUTABLE: %@%', OLD.asset_id, OLD.version;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_assets_content_immutable
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION maestro_asset_content_immutable();

-- Status may only walk draft -> reviewed -> approved -> superseded
-- (WGM-INV-013); reverting means registering a new version.
CREATE OR REPLACE FUNCTION maestro_asset_status_transition() RETURNS trigger AS $$
BEGIN
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('draft', 'reviewed')) OR
        (OLD.status = 'reviewed' AND NEW.status IN ('reviewed', 'approved')) OR
        (OLD.status = 'approved' AND NEW.status IN ('approved', 'superseded')) OR
        (OLD.status = 'superseded' AND NEW.status = 'superseded')
    ) THEN
        RAISE EXCEPTION 'ASSET_STATUS_TRANSITION_INVALID: %@% % -> %',
            OLD.asset_id, OLD.version, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_assets_status_transition
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION maestro_asset_status_transition();

CREATE TRIGGER trg_assets_no_delete
    BEFORE DELETE ON assets
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('ASSET');

-- consumes/produces edges between graph nodes and ledger assets.
CREATE TABLE node_artifact_flows (
    id            uuid PRIMARY KEY,
    plan_id       uuid NOT NULL REFERENCES work_plans (id),
    node_id       uuid NOT NULL,
    direction     text NOT NULL CHECK (direction IN ('consumes', 'produces')),
    asset_id      text NOT NULL,
    asset_version integer NOT NULL CHECK (asset_version > 0),
    port_key      text NOT NULL CHECK (port_key ~ '^[a-z][a-z0-9.]{0,63}$'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, direction, port_key),
    FOREIGN KEY (node_id, plan_id) REFERENCES work_nodes (id, plan_id),
    FOREIGN KEY (asset_id, asset_version) REFERENCES assets (asset_id, version)
);

CREATE INDEX idx_node_artifact_flows_node ON node_artifact_flows (plan_id, node_id);

CREATE TABLE asset_gate_bindings (
    id            uuid PRIMARY KEY,
    project_id    uuid NOT NULL REFERENCES projects (id),
    work_item_id  uuid NOT NULL REFERENCES work_items (id),
    asset_id      text NOT NULL,
    bound_version integer NOT NULL CHECK (bound_version > 0),
    bound_digest  text NOT NULL CHECK (bound_digest ~ '^sha256:[0-9a-f]{64}$'),
    gate_id       text NOT NULL CHECK (char_length(gate_id) BETWEEN 1 AND 128),
    status        text NOT NULL DEFAULT 'bound' CHECK (status IN ('bound', 'stale')),
    bound_at      timestamptz NOT NULL DEFAULT now(),
    staled_at     timestamptz,
    UNIQUE (work_item_id, asset_id, gate_id),
    FOREIGN KEY (asset_id, bound_version) REFERENCES assets (asset_id, version),
    CHECK (status = 'bound' OR staled_at IS NOT NULL)
);

CREATE INDEX idx_asset_gate_bindings_item ON asset_gate_bindings (project_id, work_item_id);
CREATE INDEX idx_asset_gate_bindings_asset ON asset_gate_bindings (asset_id, bound_version);
CREATE INDEX idx_asset_gate_bindings_status ON asset_gate_bindings (status) WHERE status = 'stale';
