-- Reverts 0004_m2_gitlab by dropping the M2 tables. Pre-cutover drill
-- only; production recovery is PITR/forward-fix.
DROP TABLE IF EXISTS waivers, gate_snapshots, evidence, pipeline_jobs, pipelines, merge_requests, webhook_deliveries, webhook_inbox, gitlab_project_mappings, gitlab_instances CASCADE;
