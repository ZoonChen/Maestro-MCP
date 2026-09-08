-- Evaluation records: persist the frozen wire's trajectory_constraints.
--
-- The frozen evaluation-record.schema.json carries
-- trajectory_constraints (the constraint ids the trajectory layer
-- checked the run against); migration 0012 built the queryable
-- projection without that column, so writing a wire record would have
-- silently dropped the field. This closes the gap additively.

ALTER TABLE evaluation_records
    ADD COLUMN trajectory_constraints jsonb NOT NULL DEFAULT '[]'::jsonb;
