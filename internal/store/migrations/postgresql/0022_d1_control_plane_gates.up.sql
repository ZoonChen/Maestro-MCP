-- ---------------------------------------------------------------------------
-- D1 control-plane self-attesting gates (F14 structural fix, one of three)
--
-- The evidence authority enum gains 'control_plane': the evaluation
-- engine's own deterministic judgment for the three engine-oracle
-- gates (policy_integrity / baseline_freshness / boundary). Such rows
-- carry no pipeline/job identity and are written append-only like any
-- other evidence; docs/specs/schemas/evidence.schema.json is the wire
-- authority for the conditional rules.
-- ---------------------------------------------------------------------------

ALTER TABLE evidence DROP CONSTRAINT IF EXISTS evidence_authority_check;

ALTER TABLE evidence ADD CONSTRAINT evidence_authority_check
    CHECK (authority IN ('diagnostic', 'merge_gate', 'control_plane'));
