-- Best-effort down for W7: reverting the executions enum fails while
-- 'released' rows exist; park them as interrupted first (the lease
-- trail keeps the release lineage — leases.status already 'released').

DROP INDEX IF EXISTS idx_validation_runs_idempotency;

ALTER TABLE validation_runs DROP COLUMN IF EXISTS idempotency_key;

UPDATE executions SET status = 'interrupted' WHERE status = 'released';

ALTER TABLE executions DROP CONSTRAINT IF EXISTS executions_status_check;

ALTER TABLE executions ADD CONSTRAINT executions_status_check
    CHECK (status IN ('running', 'completed', 'failed', 'cancelled', 'interrupted'));

ALTER TABLE work_items DROP COLUMN IF EXISTS queue_requeued_at;
