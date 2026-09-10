-- W4.5 J3 Jira connector (task brief J3 / SOLUTION-BLUEPRINT section 1).
--
-- Authority: plans/prep/pilot/SOLUTION-BLUEPRINT.md section 1.1/1.2 (SoR
-- matrix and sync semantics). Maestro is the SoR for the work graph;
-- Jira issues are the kanban view. Two tables:
--
--   jira_anchors: the WorkItem <-> issue bidirectional id anchor (section
--     1.2 rule 3: anchoring is established at creation; an unanchored
--     Jira issue produces no Maestro side effect). The row carries the
--     SoR-side desired mirror fields (Maestro-owned) and the Jira-side
--     read-only snapshot; the snapshot NEVER feeds work_items.status
--     (section 1.2 rule 1).
--
--   jira_reconcile_items: one detected field divergence between the SoR
--     value and the Jira-side snapshot (section 1.2 rule 2: SoR wins,
--     the divergence waits for human adjudication; two reconcile cycles
--     unresolved escalate to the owner).

CREATE TABLE jira_anchors (
    id               uuid PRIMARY KEY,
    project_id       uuid NOT NULL REFERENCES projects (id),
    work_item_id     uuid NOT NULL,
    issue_key        text NOT NULL CHECK (issue_key ~ '^[A-Z][A-Z0-9_]*-[0-9]+$'),
    jira_project_key text NOT NULL CHECK (jira_project_key ~ '^[A-Z][A-Z0-9_]*$'),
    -- anchor_source: how the anchor was established (manual = human
    -- keyed the pair; api = a server-side surface created it).
    anchor_source    text NOT NULL CHECK (anchor_source IN ('manual', 'api')),
    -- SoR-side desired mirror values (Maestro-owned; the mirror worker
    -- pushes exactly these). title/status derive from work_items at
    -- mirror time and are not stored here.
    assignee         text NOT NULL DEFAULT '',
    iteration_label  text NOT NULL DEFAULT '',
    -- Jira-side read-only snapshot (reverse channel; feeds divergence
    -- detection only). issue_status is display-only by construction.
    issue_title      text NOT NULL DEFAULT '',
    issue_assignee   text NOT NULL DEFAULT '',
    issue_labels     jsonb NOT NULL DEFAULT '[]'::jsonb,
    issue_status     text NOT NULL DEFAULT '',
    snapshot_at      timestamptz,
    -- What Maestro last established on the Jira side (per mirror
    -- field). NULL = not established (or re-armed by an accept_sor
    -- adjudication): the mirror pushes. The pushed memory is what
    -- separates the two directions — a SoR change pushes, a Jira-side
    -- drift does NOT (it belongs to the reconcile list, and only a
    -- human adjudication may overwrite it).
    pushed_title        text,
    pushed_assignee     text,
    pushed_iteration    text,
    pushed_status_label text,
    -- mirror bookkeeping (degraded states are honest, never blocking).
    last_mirror_at   timestamptz,
    last_mirror_ok   boolean,
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, work_item_id),
    UNIQUE (project_id, issue_key),
    FOREIGN KEY (project_id, work_item_id) REFERENCES work_items (project_id, id)
);

CREATE INDEX idx_jira_anchors_project ON jira_anchors (project_id);

CREATE TABLE jira_reconcile_items (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects (id),
    anchor_id       uuid NOT NULL REFERENCES jira_anchors (id),
    -- field: which mirror value diverged. status_label covers the
    -- maestro:<status> label channel (the only status coupling, and it
    -- only ever flows Maestro -> Jira).
    field           text NOT NULL CHECK (field IN ('title', 'assignee', 'iteration_label', 'status_label')),
    sor_value       text NOT NULL,
    mirror_value    text NOT NULL,
    -- open -> resolved (human adjudication, or the divergence healed);
    -- two reconcile cycles unresolved -> escalated (+ audit event);
    -- adjudicating an escalated item still resolves it.
    state           text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved', 'escalated')),
    open_cycles     integer NOT NULL DEFAULT 0 CHECK (open_cycles >= 0),
    detected_at     timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    resolved_by     text,
    resolved_at     timestamptz,
    resolution      text CHECK (resolution IN ('accept_sor', 'accept_mirror') OR resolution IS NULL),
    resolution_note text,
    CHECK ((state = 'resolved') = (resolved_at IS NOT NULL)),
    CHECK ((state = 'resolved') = (resolution IS NOT NULL)),
    CHECK (resolution IS NULL OR resolved_by IS NOT NULL)
);

-- At most one live (open/escalated) divergence per anchor field; a
-- resolved history row never blocks a fresh detection.
CREATE UNIQUE INDEX idx_jira_reconcile_one_live_per_field
    ON jira_reconcile_items (anchor_id, field) WHERE state IN ('open', 'escalated');

CREATE INDEX idx_jira_reconcile_project_state ON jira_reconcile_items (project_id, state);
