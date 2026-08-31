-- M2 merged-fact lineage (P4, S4a).
--
-- Authority: docs/technical/gitlab-integration.md GL-INV-003 (done is
-- confirmed only by a merged webhook or API reconciliation, recording
-- the source event and the merge SHA) and the frozen ready_for_human_
-- merge -> done edge, which only the fact-bound path may drive. The
-- source event identity is the webhook inbox delivery (instance,
-- external_event_id) or the reconcile run id.

ALTER TABLE work_items ADD COLUMN merged_fact_id text;
