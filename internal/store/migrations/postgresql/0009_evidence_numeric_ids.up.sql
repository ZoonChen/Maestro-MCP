-- M2 evidence numeric provider ids (P4, S4b quality loop).
--
-- Authority: the frozen evidence.schema.json carries pipeline_id and
-- job_id as INTEGERS (the GitLab numeric identities); 0004 modeled
-- them as uuid foreign keys to the projection tables, which the
-- original README deviation note deferred to the S4a connector. The
-- projection rows exist now, but the wire contract wants the numeric
-- ids the producer reported — store both: numeric ids here (wire),
-- uuid FK backfill remains a reconcile-time enrichment.

ALTER TABLE evidence ADD COLUMN gitlab_pipeline_id bigint CHECK (gitlab_pipeline_id IS NULL OR gitlab_pipeline_id >= 1);
ALTER TABLE evidence ADD COLUMN gitlab_job_id bigint CHECK (gitlab_job_id IS NULL OR gitlab_job_id >= 1);
