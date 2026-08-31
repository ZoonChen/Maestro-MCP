-- M2 GitLab integration tables (P3, S4-led).
--
-- Authority: docs/technical/gitlab-integration.md (connector/webhook/sync),
-- docs/quality/gates-and-evidence.md (Evidence/Gate/Waiver), ADR-005
-- (human-only merge), ADR-006 (CI evidence authority), the frozen
-- events.yaml 3.0.0 (webhook payload contract absorbing the S4 CE
-- deviations), and the M2 delivery book task list. Every table below
-- traces to DATA-REQ-002/003 or the M2 anchor cards; deviations are
-- recorded in migrations/postgresql/README.md.

-- Approved GitLab instances: HTTPS hosts only, exact numeric project
-- bindings, credentials only as secret references.
CREATE TABLE gitlab_instances (
    id                     uuid PRIMARY KEY,
    base_url               text NOT NULL UNIQUE CHECK (base_url ~ '^https://'),
    display_name           text NOT NULL,
    status                 text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended', 'removed')),
    bot_credential_ref     text NOT NULL,
    webhook_secret_ref     text NOT NULL,
    registered_webhook_id  text,
    last_health_at         timestamptz,
    last_health_ok         boolean,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- Numeric project bindings: (instance_id, gitlab_project_id) is unique;
-- project scope comes from the Maestro projects table.
CREATE TABLE gitlab_project_mappings (
    gitlab_instance_id  uuid NOT NULL REFERENCES gitlab_instances (id),
    gitlab_project_id   bigint NOT NULL CHECK (gitlab_project_id >= 1),
    project_id          uuid NOT NULL,
    default_branch      text NOT NULL DEFAULT '',
    repository_url      text NOT NULL DEFAULT '',
    webhook_uuid        text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (gitlab_instance_id, gitlab_project_id),
    FOREIGN KEY (project_id) REFERENCES projects (id)
);
CREATE INDEX idx_gitlab_mappings_project ON gitlab_project_mappings (project_id);

-- Durable webhook Inbox: the payload is UNTRUSTED input (CE token mode
-- has no HMAC); verification is constant-time token compare + TLS +
-- received_at short window + (instance_id, external_event_id) dedup —
-- the S4 CE deviations absorbed by the frozen event catalog.
CREATE TABLE webhook_inbox (
    id                    uuid PRIMARY KEY,
    gitlab_instance_id    uuid NOT NULL REFERENCES gitlab_instances (id),
    external_event_id     text NOT NULL,
    event_kind            text NOT NULL CHECK (event_kind IN ('push', 'merge_request', 'pipeline', 'job')),
    webhook_uuid          text,
    payload_digest        text NOT NULL CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    raw_body_encrypted    bytea,
    received_at           timestamptz NOT NULL DEFAULT now(),
    status                text NOT NULL DEFAULT 'received' CHECK (status IN (
        'received', 'processing', 'processed', 'retry_wait', 'dead_letter')),
    attempts              integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    processed_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (gitlab_instance_id, external_event_id)
);
CREATE INDEX idx_webhook_inbox_dispatch ON webhook_inbox (status, received_at);

-- Append-only raw delivery log for replay forensics; rows are never
-- updated or deleted (quarantine moves to dead_letter status on the
-- inbox row itself, this table only records the audit trail).
CREATE TABLE webhook_deliveries (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    inbox_id              uuid NOT NULL,
    gitlab_instance_id    uuid NOT NULL,
    external_event_id     text NOT NULL,
    event_kind           text NOT NULL,
    token_verified        boolean NOT NULL,
    outcome               text NOT NULL CHECK (outcome IN ('accepted', 'duplicate', 'rejected', 'dead_letter')),
    reject_reason         text,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_webhook_deliveries_inbox ON webhook_deliveries (inbox_id);

-- Merge-request sync: the task marker links MR state to work items;
-- merged facts carry the SHA tuple that opens the done edge.
CREATE TABLE merge_requests (
    id                          uuid PRIMARY KEY,
    project_id             uuid NOT NULL,
    gitlab_instance_id     uuid NOT NULL REFERENCES gitlab_instances (id),
    gitlab_project_id      bigint NOT NULL,
    mr_iid                  bigint NOT NULL CHECK (mr_iid >= 1),
    work_item_id            uuid,
    state                   text NOT NULL CHECK (state IN ('opened', 'closed', 'merged', 'locked')),
    detailed_merge_status  text,
    source_branch           text NOT NULL,
    target_branch           text NOT NULL,
    source_sha              text,
    target_sha              text,
    merge_commit_sha        text,
    merged_at               timestamptz,
    observed_at             timestamptz NOT NULL DEFAULT now(),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (gitlab_instance_id, gitlab_project_id, mr_iid),
    FOREIGN KEY (project_id, work_item_id) REFERENCES work_items (project_id, id)
);
CREATE INDEX idx_merge_requests_project ON merge_requests (project_id);
CREATE INDEX idx_merge_requests_work_item ON merge_requests (work_item_id);

-- Pipeline and job projections: exact-SHA quality evidence sources.
CREATE TABLE pipelines (
    id                     uuid PRIMARY KEY,
    project_id            uuid NOT NULL,
    gitlab_instance_id    uuid NOT NULL REFERENCES gitlab_instances (id),
    gitlab_project_id     bigint NOT NULL,
    gitlab_pipeline_id    bigint NOT NULL,
    sha                   text NOT NULL,
    ref                   text NOT NULL,
    status                text NOT NULL,
    source                text,
    web_url               text,
    observed_at           timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (gitlab_instance_id, gitlab_pipeline_id),
    FOREIGN KEY (project_id) REFERENCES projects (id)
);
CREATE INDEX idx_pipelines_project_sha ON pipelines (project_id, sha);

CREATE TABLE pipeline_jobs (
    id                     uuid PRIMARY KEY,
    pipeline_id            uuid NOT NULL REFERENCES pipelines (id),
    gitlab_job_id          bigint NOT NULL,
    name                   text NOT NULL,
    status                 text NOT NULL,
    stage                  text,
    started_at             timestamptz,
    finished_at            timestamptz,
    observed_at            timestamptz NOT NULL DEFAULT now(),
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (pipeline_id, gitlab_job_id)
);

-- Authoritative CI evidence: append-only with supersede chains
-- (DATA-REQ-003). authority=merge_gate can only originate from verified
-- GitLab ingestion; diagnostic rows come from local runner validation.
CREATE TABLE evidence (
    id                  uuid PRIMARY KEY,
    project_id          text NOT NULL,
    work_item_id        text NOT NULL,
    authority           text NOT NULL CHECK (authority IN ('diagnostic', 'merge_gate')),
    producer            text NOT NULL,
    evidence_kind       text NOT NULL,
    source_sha          text NOT NULL,
    target_sha          text NOT NULL,
    pipeline_id         uuid REFERENCES pipelines (id),
    job_id              uuid REFERENCES pipeline_jobs (id),
    payload_digest      text NOT NULL CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_version      text NOT NULL,
    supersedes_id       uuid,
    observed_at         timestamptz NOT NULL DEFAULT now(),
    created_at           timestamptz NOT NULL DEFAULT now(),
    -- The pipeline link is a plain FK: evidence carries SHA tuples while
    -- pipelines is keyed by uuid; the application enforces that the
    -- pipeline's sha matches evidence.source_sha (README deviation note).
    CONSTRAINT fk_evidence_pipeline FOREIGN KEY (pipeline_id) REFERENCES pipelines (id)
);

CREATE INDEX idx_evidence_work_item ON evidence (project_id, work_item_id);
CREATE INDEX idx_evidence_supersedes ON evidence (supersedes_id);

-- Gate snapshots: the evaluated state of a quality gate at an exact SHA
-- tuple under a named policy version (ADR-006: CI evidence is the
-- authority; local diagnostics never satisfy merge gates).
CREATE TABLE gate_snapshots (
    id                  uuid PRIMARY KEY,
    project_id          text NOT NULL,
    work_item_id        text NOT NULL,
    gate_id             text NOT NULL,
    status              text NOT NULL CHECK (status IN (
        'pending', 'running', 'passed', 'failed', 'error', 'stale', 'waived')),
    source_sha          text NOT NULL,
    target_sha          text NOT NULL,
    policy_version      text NOT NULL,
    evidence_ids        jsonb NOT NULL DEFAULT '[]'::jsonb,
    evaluated_at        timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, work_item_id, gate_id, source_sha, target_sha, policy_version)
);
CREATE INDEX idx_gate_snapshots_work_item ON gate_snapshots (project_id, work_item_id);

-- Gate waivers: independent approver, bounded lifetime, single SHA
-- binding; non-waivable principles are enforced in the engine, not here.
CREATE TABLE waivers (
    id                  uuid PRIMARY KEY,
    project_id          text NOT NULL,
    work_item_id        text NOT NULL,
    gate_id             text NOT NULL,
    source_sha          text NOT NULL,
    state               text NOT NULL DEFAULT 'requested' CHECK (state IN (
        'requested', 'approved', 'rejected', 'active', 'expired', 'revoked')),
    approver_principal  text,
    requester_principal text NOT NULL,
    reason              text NOT NULL,
    expires_at          timestamptz NOT NULL,
    approved_at         timestamptz,
    revoked_at          timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, work_item_id, gate_id, source_sha)
);
CREATE INDEX idx_waivers_work_item ON waivers (project_id, work_item_id);

-- Append-only enforcement: evidence rows are immutable facts, and the
-- inbox/delivery audit trail never rewrites history.
CREATE TRIGGER trg_evidence_immutable
    BEFORE UPDATE OR DELETE ON evidence
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('EVIDENCE');
CREATE TRIGGER trg_webhook_deliveries_immutable
    BEFORE UPDATE OR DELETE ON webhook_deliveries
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('WEBHOOK_DELIVERY');
