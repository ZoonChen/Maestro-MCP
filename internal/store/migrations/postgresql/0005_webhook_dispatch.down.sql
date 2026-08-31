ALTER TABLE webhook_deliveries ALTER COLUMN inbox_id SET NOT NULL;
DROP INDEX IF EXISTS idx_webhook_inbox_claim;
ALTER TABLE webhook_inbox
    DROP COLUMN IF EXISTS lease_owner,
    DROP COLUMN IF EXISTS claimed_at,
    DROP COLUMN IF EXISTS next_attempt_at;
