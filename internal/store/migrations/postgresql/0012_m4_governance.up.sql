-- M4 governance tables (P3, S1-led).
--
-- Authority: docs/delivery/m4-governance-console.md (the approved M4
-- book), the frozen evaluation-record / audit-export / slo-status
-- machine schemas (I4) and the operations authority documents. Every
-- table traces to the M4 anchor cards (OBS/EVAL/PILOT); deviations
-- are recorded in migrations/postgresql/README.md.

-- OBS: privacy-masked telemetry aggregates. Raw events are NEVER
-- stored here — the producer applies the redaction policy version
-- before emitting, and the policy identity is carried for audit.
CREATE TABLE telemetry_aggregates (
    id                uuid PRIMARY KEY,
    project_id        uuid NOT NULL REFERENCES projects (id),
    metric            text NOT NULL CHECK (char_length(metric) BETWEEN 1 AND 128),
    window_kind       text NOT NULL CHECK (window_kind IN ('1m', '5m', '1h', '1d')),
    window_start      timestamptz NOT NULL,
    sample_count      bigint NOT NULL CHECK (sample_count >= 0),
    sum_value         double precision NOT NULL,
    min_value         double precision,
    max_value         double precision,
    p50_value         double precision,
    p95_value         double precision,
    p99_value         double precision,
    redaction_version text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, metric, window_kind, window_start)
);
CREATE INDEX idx_telemetry_project_metric ON telemetry_aggregates (project_id, metric, window_start DESC);

-- EVAL: the four-layer harness records (evaluation-record.schema.json
-- is the wire shape; the columns carry the queryable projection).
CREATE TABLE evaluation_records (
    id                        uuid PRIMARY KEY,
    project_id               uuid,
    layer                    text NOT NULL CHECK (layer IN ('quality', 'trajectory', 'security', 'capability')),
    case_id                  text NOT NULL CHECK (char_length(case_id) BETWEEN 1 AND 128),
    run_id                   uuid NOT NULL,
    category                 text,
    risk                     text CHECK (risk IN ('benign', 'low', 'elevated', 'high')),
    verdict                  text NOT NULL CHECK (verdict IN ('pass', 'fail', 'error', 'blocked', 'skipped')),
    score                    double precision NOT NULL CHECK (score BETWEEN 0 AND 100),
    scorer_kind              text NOT NULL CHECK (scorer_kind IN ('deterministic', 'rule_based', 'llm_judge', 'human')),
    scorer_version           text NOT NULL,
    calibration_ref          text,
    dataset_digest           text NOT NULL CHECK (dataset_digest ~ '^sha256:[0-9a-f]{64}$'),
    model_config_digest      text,
    seed                     bigint,
    forbidden_actions_observed jsonb NOT NULL DEFAULT '[]'::jsonb,
    tokens_used              bigint NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    latency_ms               bigint,
    external_state_digest    text,
    evidence_refs            jsonb NOT NULL DEFAULT '[]'::jsonb,
    notes                    text,
    version                  bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, case_id, layer),
    -- The frozen wire schema: security records require risk (the schema's
    -- conditional requirement becomes a table invariant).
    CHECK (layer <> 'security' OR risk IS NOT NULL),
    FOREIGN KEY (project_id) REFERENCES projects (id)
);
CREATE INDEX idx_evaluation_layer_run ON evaluation_records (layer, run_id);
CREATE INDEX idx_evaluation_dataset ON evaluation_records (dataset_digest);

-- PILOT: rollout flags — shadow first, then percentage grayscale, and
-- a project can never exceed 100% (the CHECK carries the enum order).
CREATE TABLE pilot_flags (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects (id),
    flag            text NOT NULL CHECK (char_length(flag) BETWEEN 1 AND 128),
    stage           text NOT NULL DEFAULT 'off' CHECK (stage IN ('off', 'shadow', 'gray', 'full', 'rolled_back')),
    gray_percent    integer NOT NULL DEFAULT 0 CHECK (gray_percent BETWEEN 0 AND 100),
    changed_by      text NOT NULL,
    reason          text NOT NULL CHECK (char_length(reason) BETWEEN 8 AND 2000),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, flag),
    -- gray_percent only applies in the gray stage; shadow/full carry 0.
    CHECK ((stage = 'gray') OR (gray_percent = 0))
);
