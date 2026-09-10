-- Reverse of 0019: drop the J2b protocol tables and restore the
-- budget ledger scope catalog. Fully reversible; no data transform.

ALTER TABLE budget_ledgers
    DROP CONSTRAINT budget_ledgers_scope_kind_check;
ALTER TABLE budget_ledgers
    ADD CONSTRAINT budget_ledgers_scope_kind_check
    CHECK (scope_kind IN ('defect', 'work_item', 'agent_run'));

DROP TRIGGER IF EXISTS trg_execution_attempts_no_delete ON execution_attempts;
DROP TRIGGER IF EXISTS trg_execution_attempts_binding_immutable ON execution_attempts;
DROP FUNCTION IF EXISTS maestro_attempt_binding_immutable();
DROP INDEX IF EXISTS idx_execution_attempts_expiry;
DROP INDEX IF EXISTS idx_execution_attempts_plan;
DROP INDEX IF EXISTS uniq_execution_attempts_active;
DROP TABLE IF EXISTS execution_attempts;

DROP TRIGGER IF EXISTS trg_decomposition_proposals_decided_immutable ON decomposition_proposals;
DROP FUNCTION IF EXISTS maestro_proposal_decided_immutable();
DROP INDEX IF EXISTS idx_decomposition_proposals_plan;
DROP INDEX IF EXISTS uniq_decomposition_proposals_key;
DROP TABLE IF EXISTS decomposition_proposals;

DROP TRIGGER IF EXISTS trg_work_patterns_body_immutable ON work_patterns;
DROP FUNCTION IF EXISTS maestro_work_pattern_body_immutable();
DROP INDEX IF EXISTS idx_work_patterns_project;
DROP TABLE IF EXISTS work_patterns;

DROP TABLE IF EXISTS work_plan_intents;
DROP TABLE IF EXISTS problem_capability_links;
DROP TABLE IF EXISTS capabilities;
DROP TRIGGER IF EXISTS trg_outcome_contracts_immutable ON outcome_contracts;
DROP TABLE IF EXISTS outcome_contracts;
DROP INDEX IF EXISTS idx_business_problems_project;
DROP TABLE IF EXISTS business_problems;
