-- M3 agent-run resume identity (P4, S5b-2).
--
-- Authority: docs/prd/agent-remediation.md section 8 (crash recovery
-- resumes from the persisted state; side-effecting calls never replay)
-- and the orchestrator's CreateRun find-or-resume contract. 0010
-- modeled agent_runs without a uniqueness key on the defect+attempt
-- resume identity, so a crashed creator could duplicate a run.

CREATE UNIQUE INDEX one_live_run_per_defect_attempt
    ON agent_runs (project_id, defect_id, attempt);
