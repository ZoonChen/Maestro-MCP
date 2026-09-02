-- M3 contract/finding/defect/budget/agent tables (P3, S5a-led).
--
-- Authority: docs/delivery/m3-integration-defect-automation.md (the
-- approved M3 book), the frozen events.yaml 3.1.0 payload contracts
-- (finding.created / defect.uniqued / agent.run.handoff /
-- budget.exhausted), the four new machine schemas (finding / defect /
-- integration-run / budget-ledger) and the S5 prep DDL review
-- (plans/prep/m3/p3-data-model-design.md). Every table traces to the
-- M3 anchor cards (CTR/INT/DEF/DSP/AGT/BUD). SQLite gains nothing:
-- M3 entities are PostgreSQL-only (SQLite is the M0 import source).

-- Card 1 (CTR): canonicalized, hashed API contract versions. Same
-- project+service+version can never carry two different hashes.
CREATE TABLE api_contracts (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects (id),
    service         text NOT NULL CHECK (char_length(service) BETWEEN 1 AND 100),
    format          text NOT NULL CHECK (format IN ('openapi3-json', 'openapi3-yaml')),
    version         text NOT NULL CHECK (char_length(version) BETWEEN 1 AND 64),
    canonical_hash  text NOT NULL CHECK (canonical_hash ~ '^sha256:[0-9a-f]{64}$'),
    spec_digest     text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_sha      text NOT NULL CHECK (char_length(source_sha) BETWEEN 7 AND 64),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, service, version)
);

-- Card 2 (INT): exact-combination cross-repository runs. The manifest
-- hash IS the combination identity — replays collapse on it.
CREATE TABLE integration_runs (
    id                uuid PRIMARY KEY,
    project_id        uuid NOT NULL REFERENCES projects (id),
    manifest_hash     text NOT NULL CHECK (manifest_hash ~ '^sha256:[0-9a-f]{64}$'),
    revisions         jsonb NOT NULL, -- exact (mapping, sha, contract hash) set
    environment_profile_id text,
    environment_lease jsonb,          -- lease id / TTL / teardown ref
    status            text NOT NULL CHECK (status IN
        ('waiting', 'running', 'passed', 'failed', 'cancelled', 'expired')),
    phase             text NOT NULL DEFAULT 'queued' CHECK (phase IN
        ('queued', 'contract_check', 'provisioning', 'executing', 'cleanup', 'complete')),
    cleanup_status    text NOT NULL DEFAULT 'not_started' CHECK (cleanup_status IN
        ('not_started', 'running', 'passed', 'failed')),
    combination_digest text NOT NULL CHECK (combination_digest ~ '^sha256:[0-9a-f]{64}$'),
    evidence_refs     jsonb NOT NULL DEFAULT '[]'::jsonb,
    responsibility    jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, manifest_hash)
);
CREATE INDEX idx_integration_runs_project ON integration_runs (project_id, created_at);

-- Card 3 (DEF): normalized findings from the six source adapters.
-- source_event_id is the ingest idempotency key (webhook event ID /
-- scan run ID / QA record ID).
CREATE TABLE findings (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects (id),
    source_type     text NOT NULL CHECK (source_type IN
        ('pipeline', 'junit', 'contract', 'sast', 'secret', 'manual_qa')),
    source_event_id text NOT NULL,
    severity        text NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low', 'info')),
    environment     text,
    repro           text,
    evidence_refs   jsonb NOT NULL DEFAULT '[]'::jsonb,
    task_refs       jsonb NOT NULL DEFAULT '[]'::jsonb,
    adapter_version text NOT NULL,
    payload_digest  text NOT NULL CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, source_type, source_event_id),
    UNIQUE (project_id, id)
);
CREATE INDEX idx_findings_project_severity ON findings (project_id, severity, created_at);

-- Card 4 (DSP): the unique defect. The fingerprint is versioned; a
-- fingerprint-version bump re-keys the space (old defects stay).
CREATE TABLE defects (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects (id),
    fingerprint_version integer NOT NULL CHECK (fingerprint_version BETWEEN 1 AND 3),
    fingerprint_hash    text NOT NULL CHECK (fingerprint_hash ~ '^sha256:[0-9a-f]{64}$'),
    state               text NOT NULL CHECK (state IN
        ('detected', 'triaged', 'assigned', 'reproducing', 'fixing', 'verifying',
         'resolved', 'reopened', 'quarantined', 'ignored')),
    severity            text NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low', 'info')),
    title               text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    sla_due_at          timestamptz,
    responsible_task_id text,
    first_seen_at       timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    resolved_at         timestamptz,
    occurrence          integer NOT NULL DEFAULT 1 CHECK (occurrence >= 1),
    version             bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, fingerprint_version, fingerprint_hash),
    UNIQUE (project_id, id)
);
CREATE INDEX idx_defects_project_state ON defects (project_id, state, severity);

-- Occurrences are append-only links: a defect's history never rewrites.
CREATE TABLE defect_occurrences (
    id          uuid PRIMARY KEY,
    project_id  uuid NOT NULL,
    defect_id   uuid NOT NULL REFERENCES defects (id),
    finding_id  uuid NOT NULL REFERENCES findings (id),
    branch      text,
    commit_sha  text,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (defect_id, finding_id),
    FOREIGN KEY (project_id, defect_id) REFERENCES defects (project_id, id),
    FOREIGN KEY (project_id, finding_id) REFERENCES findings (project_id, id),
    UNIQUE (project_id, id)
);

-- Card 4/5 (DSP/AGT): defect-to-task routing. One active fix link per
-- defect — a second remediation must wait or take over explicitly.
CREATE TABLE defect_task_links (
    id           uuid PRIMARY KEY,
    project_id   uuid NOT NULL,
    defect_id    uuid NOT NULL,
    work_item_id uuid NOT NULL,
    link_kind    text NOT NULL CHECK (link_kind IN ('responsibility', 'fix', 'verify')),
    active       boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (defect_id, work_item_id, link_kind),
    FOREIGN KEY (project_id, defect_id) REFERENCES defects (project_id, id),
    FOREIGN KEY (project_id, work_item_id) REFERENCES work_items (project_id, id)
);
CREATE UNIQUE INDEX one_active_fix_per_defect
    ON defect_task_links (defect_id) WHERE link_kind = 'fix' AND active;

-- Card 6 (BUD): the pre-call gated, truly accounted budget ledger.
CREATE TABLE budget_ledgers (
    id             uuid PRIMARY KEY,
    project_id     uuid NOT NULL REFERENCES projects (id),
    scope_kind     text NOT NULL CHECK (scope_kind IN ('defect', 'work_item', 'agent_run')),
    scope_id       uuid NOT NULL,
    budget_version text NOT NULL,
    budget_units   bigint NOT NULL CHECK (budget_units >= 1),
    reserved_units bigint NOT NULL DEFAULT 0 CHECK (reserved_units >= 0),
    spent_units    bigint NOT NULL DEFAULT 0 CHECK (spent_units >= 0),
    max_attempts   integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    wall_time_limit_seconds integer NOT NULL DEFAULT 1800 CHECK (wall_time_limit_seconds >= 60),
    state          text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'stopped', 'closed')),
    stop_reason    text CHECK (stop_reason IS NULL OR stop_reason IN
        ('budget_exhausted', 'attempt_limit', 'time_limit', 'manual_stop', 'policy_stop')),
    stopped_at     timestamptz,
    version        bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_budget_ledgers_scope ON budget_ledgers (scope_kind, scope_id);

-- Entries are append-only real usage: reserve/release/spend from the
-- provider's own accounting, never our estimate.
CREATE TABLE budget_entries (
    id             uuid PRIMARY KEY,
    ledger_id      uuid NOT NULL REFERENCES budget_ledgers (id),
    entry_seq      bigint NOT NULL CHECK (entry_seq >= 1),
    direction      text NOT NULL CHECK (direction IN ('reserve', 'release', 'spend')),
    units          bigint NOT NULL CHECK (units >= 1),
    tool_ref       text,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (ledger_id, entry_seq)
);

-- Card 5 (AGT): remediation runs with checkpoint binding. Handoffs keep
-- the checkpoint digest so a human (or a later attempt) resumes rather
-- than restarts.
CREATE TABLE agent_runs (
    id                uuid PRIMARY KEY,
    project_id        uuid NOT NULL REFERENCES projects (id),
    defect_id         uuid,
    work_item_id      uuid,
    attempt           integer NOT NULL CHECK (attempt BETWEEN 1 AND 3),
    config_digest     text NOT NULL,
    state             text NOT NULL CHECK (state IN
        ('eligibility_check', 'reproducing', 'diagnosing', 'modifying', 'local_verifying',
         'retrying', 'mr_created', 'ci_verifying', 'awaiting_human', 'needs_human', 'stopped')),
    checkpoint_digest text,
    handoff_reason    text CHECK (handoff_reason IS NULL OR handoff_reason IN
        ('cannot_reproduce', 'budget_exhausted', 'low_confidence', 'high_risk',
         'tool_limit', 'policy_stop', 'human_review_required')),
    used_tokens       bigint NOT NULL DEFAULT 0 CHECK (used_tokens >= 0),
    budget_tokens     bigint NOT NULL CHECK (budget_tokens >= 1),
    tool_trace_ref    text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (project_id, defect_id) REFERENCES defects (project_id, id),
    FOREIGN KEY (project_id, work_item_id) REFERENCES work_items (project_id, id)
);
CREATE INDEX idx_agent_runs_defect ON agent_runs (project_id, defect_id, attempt);
