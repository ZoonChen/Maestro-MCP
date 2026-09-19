-- ---------------------------------------------------------------------------
-- W7 slice-friction fixes (F29 / F32 / F15 companion schema):
--
-- 1. work_items.queue_requeued_at: the release face's queue-head key
--    (W7-1, F29). Dispatch orders by priority, then FIFO by created_at;
--    a released work item must re-enter at the HEAD of its priority
--    band, not at its original created_at position. A NULL value keeps
--    the item's natural FIFO position; a release stamps now(), and the
--    claim SELECT sorts requeued rows (newest first) ahead of
--    unrequeued rows inside the same band.
-- 2. executions status gains 'released': the runner returned the lease
--    before any terminal outcome (the new running -> queued legal edge
--    on the work item; TASK state machine doc registers the edge). The
--    leases enum already carried 'released'.
-- 3. validation_runs.idempotency_key: the W7-3 (F15) reporting face's
--    collapse anchor — the same key replays the SAME row (200), a new
--    key mints the next attempt. Legacy/imported rows keep '' and are
--    outside the partial unique index.
-- ---------------------------------------------------------------------------

ALTER TABLE work_items ADD COLUMN queue_requeued_at timestamptz;

ALTER TABLE executions DROP CONSTRAINT IF EXISTS executions_status_check;

ALTER TABLE executions ADD CONSTRAINT executions_status_check
    CHECK (status IN ('running', 'completed', 'failed', 'cancelled', 'interrupted', 'released'));

ALTER TABLE validation_runs ADD COLUMN idempotency_key text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_validation_runs_idempotency
    ON validation_runs (project_id, work_item_id, idempotency_key)
    WHERE idempotency_key <> '';
