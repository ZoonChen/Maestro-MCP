-- J2b decomposition protocol + execution binding (task brief J2b;
-- ADR-009 approved 2026-09-10, 写实决策 #2: the Intent layer tables and
-- the ExecutionAttempt five-way binding deferred from 0017 land here).
--
-- Expand-only again: seven NEW tables plus ONE constraint extension on
-- budget_ledgers (scope_kind gains 'work_node' so graph nodes can carry
-- durable budget ledgers — additive, existing rows stay valid). No
-- 0017 table or column is touched.
--
--   business_problems / outcome_contracts / capabilities /
--   work_plan_intents: the Intent layer (WGM layer 1).
--   work_patterns: versioned decomposition templates referenced by
--     every DecompositionProposal.
--   decomposition_proposals: the Coordinator-facing protocol ledger —
--     every submitted proposal with its stable rejection codes.
--   execution_attempts: the Runtime binding — one row per attempt,
--     pinned to (node_revision, spec_digest, principal, role, session,
--     worker, worktree, context_digest); binding columns are immutable
--     by trigger, terminal status is final, retry = a new row.

-- ---------------------------------------------------------------------------
-- Intent layer
-- ---------------------------------------------------------------------------

CREATE TABLE business_problems (
    id         uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    title      text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    statement  text NOT NULL CHECK (char_length(statement) BETWEEN 1 AND 2000),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('draft', 'active', 'retired')),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_business_problems_project ON business_problems (project_id);

CREATE TABLE outcome_contracts (
    id              uuid PRIMARY KEY,
    problem_id      uuid NOT NULL REFERENCES business_problems (id),
    version         integer NOT NULL CHECK (version > 0),
    success_criteria jsonb NOT NULL CHECK (jsonb_typeof(success_criteria) = 'array'),
    constraints_doc jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (problem_id, version)
);

-- Contracts are append-only facts (a revision is a new version row).
CREATE TRIGGER trg_outcome_contracts_immutable
    BEFORE UPDATE OR DELETE ON outcome_contracts
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('OUTCOME_CONTRACT');

CREATE TABLE capabilities (
    id          uuid PRIMARY KEY,
    project_id  uuid NOT NULL REFERENCES projects (id),
    cap_key     text NOT NULL CHECK (cap_key ~ '^[a-z][a-z0-9.-]{1,63}$'),
    description text NOT NULL CHECK (char_length(description) BETWEEN 1 AND 500),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, cap_key)
);

CREATE TABLE problem_capability_links (
    problem_id    uuid NOT NULL REFERENCES business_problems (id),
    capability_id uuid NOT NULL REFERENCES capabilities (id),
    PRIMARY KEY (problem_id, capability_id)
);

-- WGP-REQ-001: one plan binds exactly one primary problem+contract.
CREATE TABLE work_plan_intents (
    plan_id              uuid NOT NULL REFERENCES work_plans (id),
    problem_id           uuid NOT NULL REFERENCES business_problems (id),
    outcome_contract_id  uuid NOT NULL REFERENCES outcome_contracts (id),
    attached_by          text NOT NULL,
    attached_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_id, problem_id),
    UNIQUE (plan_id)
);

-- ---------------------------------------------------------------------------
-- Work patterns (versioned decomposition templates)
-- ---------------------------------------------------------------------------

CREATE TABLE work_patterns (
    id         uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    name       text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{1,63}$'),
    version    integer NOT NULL CHECK (version > 0),
    status     text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'retired')),
    body       jsonb NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name, version)
);

CREATE INDEX idx_work_patterns_project ON work_patterns (project_id, status);

-- Template bodies never change once written; only the lifecycle stamp
-- moves (draft -> active -> retired; re-activating means a new version).
CREATE OR REPLACE FUNCTION maestro_work_pattern_body_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.name IS DISTINCT FROM OLD.name
       OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'WORK_PATTERN_BODY_IMMUTABLE: %@%', OLD.id, OLD.version;
    END IF;
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('draft', 'active')) OR
        (OLD.status = 'active' AND NEW.status IN ('active', 'retired')) OR
        (OLD.status = 'retired' AND NEW.status = 'retired')
    ) THEN
        RAISE EXCEPTION 'WORK_PATTERN_STATUS_TRANSITION_INVALID: %@% % -> %',
            OLD.id, OLD.version, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_work_patterns_body_immutable
    BEFORE UPDATE ON work_patterns
    FOR EACH ROW EXECUTE FUNCTION maestro_work_pattern_body_immutable();

-- ---------------------------------------------------------------------------
-- Decomposition proposals
-- ---------------------------------------------------------------------------

CREATE TABLE decomposition_proposals (
    id                    uuid PRIMARY KEY,
    project_id            uuid NOT NULL REFERENCES projects (id),
    plan_id               uuid NOT NULL REFERENCES work_plans (id),
    work_pattern_id       uuid NOT NULL REFERENCES work_patterns (id),
    expected_graph_version bigint NOT NULL CHECK (expected_graph_version > 0),
    payload               jsonb NOT NULL,
    idempotency_key       text NOT NULL,
    status                text NOT NULL DEFAULT 'submitted'
                          CHECK (status IN ('submitted', 'applied', 'rejected')),
    violations            jsonb,
    applied_node_ids      jsonb,
    submitted_by          text NOT NULL,
    decided_at            timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- Replay dedup: the same key returns the same decided proposal.
CREATE UNIQUE INDEX uniq_decomposition_proposals_key ON decomposition_proposals (idempotency_key);
CREATE INDEX idx_decomposition_proposals_plan ON decomposition_proposals (plan_id);

-- Decided proposals are protocol facts: no edits after the decision.
CREATE OR REPLACE FUNCTION maestro_proposal_decided_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.status <> 'submitted' THEN
        RAISE EXCEPTION 'DECOMPOSITION_PROPOSAL_DECIDED_IMMUTABLE: %', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_decomposition_proposals_decided_immutable
    BEFORE UPDATE OR DELETE ON decomposition_proposals
    FOR EACH ROW WHEN (OLD.status <> 'submitted')
    EXECUTE FUNCTION maestro_proposal_decided_immutable();

-- ---------------------------------------------------------------------------
-- Execution attempts (Runtime layer binding)
-- ---------------------------------------------------------------------------

CREATE TABLE execution_attempts (
    id                   uuid PRIMARY KEY,
    project_id           uuid NOT NULL REFERENCES projects (id),
    plan_id              uuid NOT NULL REFERENCES work_plans (id),
    node_id              uuid NOT NULL,
    node_revision_id     uuid NOT NULL REFERENCES work_node_revisions (id),
    spec_digest          text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    attempt_no           integer NOT NULL CHECK (attempt_no > 0),
    retry_of_attempt_id  uuid REFERENCES execution_attempts (id),
    -- The five-way binding (ADR-009 §9): identity comes from the
    -- server-side session, never self-reported by the worker.
    principal            text NOT NULL,
    role                 text NOT NULL,
    session_id           text NOT NULL,
    worker_id            text NOT NULL,
    worktree_path        text NOT NULL,
    context_digest       text NOT NULL CHECK (context_digest ~ '^sha256:[0-9a-f]{64}$'),
    context_set          jsonb NOT NULL,
    budget_ledger_id     uuid REFERENCES budget_ledgers (id),
    budget_units         bigint NOT NULL CHECK (budget_units >= 0),
    -- Lease fencing mirrors the flat leases discipline (epoch/version
    -- CAS + connection generation).
    lease_token          uuid NOT NULL UNIQUE,
    lease_epoch          bigint NOT NULL CHECK (lease_epoch > 0),
    lease_version        bigint NOT NULL DEFAULT 1 CHECK (lease_version > 0),
    connection_generation text NOT NULL,
    lease_expires_at     timestamptz NOT NULL,
    idempotency_key      text NOT NULL,
    status               text NOT NULL DEFAULT 'running'
                         CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled', 'needs_human', 'expired')),
    outcome              jsonb,
    ended_at             timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (node_id, plan_id) REFERENCES work_nodes (id, plan_id),
    UNIQUE (node_id, attempt_no),
    CHECK (status = 'running' OR ended_at IS NOT NULL),
    CHECK (retry_of_attempt_id IS NULL OR retry_of_attempt_id <> id)
);

-- WGM-INV-005: at most one active attempt per node.
CREATE UNIQUE INDEX uniq_execution_attempts_active ON execution_attempts (node_id) WHERE status = 'running';
CREATE INDEX idx_execution_attempts_plan ON execution_attempts (plan_id, node_id);
CREATE INDEX idx_execution_attempts_expiry ON execution_attempts (lease_expires_at) WHERE status = 'running';

-- The binding is written once at claim time and never re-pointed:
-- recovery may only resume THIS binding (WGM-INV-006).
CREATE OR REPLACE FUNCTION maestro_attempt_binding_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.plan_id IS DISTINCT FROM OLD.plan_id
       OR NEW.node_id IS DISTINCT FROM OLD.node_id
       OR NEW.node_revision_id IS DISTINCT FROM OLD.node_revision_id
       OR NEW.spec_digest IS DISTINCT FROM OLD.spec_digest
       OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no
       OR NEW.retry_of_attempt_id IS DISTINCT FROM OLD.retry_of_attempt_id
       OR NEW.principal IS DISTINCT FROM OLD.principal
       OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.session_id IS DISTINCT FROM OLD.session_id
       OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.worktree_path IS DISTINCT FROM OLD.worktree_path
       OR NEW.context_digest IS DISTINCT FROM OLD.context_digest
       OR NEW.context_set IS DISTINCT FROM OLD.context_set
       OR NEW.budget_ledger_id IS DISTINCT FROM OLD.budget_ledger_id
       OR NEW.budget_units IS DISTINCT FROM OLD.budget_units
       OR NEW.lease_token IS DISTINCT FROM OLD.lease_token
       OR NEW.lease_epoch IS DISTINCT FROM OLD.lease_epoch
       OR NEW.connection_generation IS DISTINCT FROM OLD.connection_generation
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key THEN
        RAISE EXCEPTION 'EXECUTION_ATTEMPT_BINDING_IMMUTABLE: %', OLD.id;
    END IF;
    IF NOT (
        (OLD.status = 'running' AND NEW.status IN
            ('running', 'succeeded', 'failed', 'cancelled', 'needs_human', 'expired')) OR
        (OLD.status = NEW.status)
    ) THEN
        RAISE EXCEPTION 'EXECUTION_ATTEMPT_STATUS_TRANSITION_INVALID: % % -> %',
            OLD.id, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_execution_attempts_binding_immutable
    BEFORE UPDATE ON execution_attempts
    FOR EACH ROW EXECUTE FUNCTION maestro_attempt_binding_immutable();

-- Attempts are execution history: no deletes, ever.
CREATE TRIGGER trg_execution_attempts_no_delete
    BEFORE DELETE ON execution_attempts
    FOR EACH ROW EXECUTE FUNCTION maestro_raise_immutable('EXECUTION_ATTEMPT');

-- ---------------------------------------------------------------------------
-- Budget ledger scope extension (additive)
-- ---------------------------------------------------------------------------

ALTER TABLE budget_ledgers
    DROP CONSTRAINT budget_ledgers_scope_kind_check;
ALTER TABLE budget_ledgers
    ADD CONSTRAINT budget_ledgers_scope_kind_check
    CHECK (scope_kind IN ('defect', 'work_item', 'agent_run', 'work_node'));
