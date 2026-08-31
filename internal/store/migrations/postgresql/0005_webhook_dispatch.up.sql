-- M2 webhook dispatch bookkeeping (P4, S4b).
--
-- Authority: docs/delivery/m2-gitlab-quality-loop.md M2-WHK-001
-- (exponential backoff with jitter, DLQ, permissioned replay) and
-- docs/security/secrets-webhooks-supply-chain.md WEBHOOK-RULE-002 /
-- section 7 (deny and replay auditing). Migration 0004 modeled the inbox
-- state machine but left three operational columns to the implementation
-- slice that first needs them; deviations are recorded in
-- migrations/postgresql/README.md.

-- Backoff scheduling: retry_wait rows become claimable again only after
-- the computed delay has elapsed.
ALTER TABLE webhook_inbox ADD COLUMN next_attempt_at timestamptz;

-- Dispatcher lease: a claimed row that never settles (crashed worker) is
-- reclaimable after a bounded stale window, mirroring the outbox lease
-- discipline.
ALTER TABLE webhook_inbox ADD COLUMN lease_owner text,
    ADD COLUMN claimed_at timestamptz;
CREATE INDEX idx_webhook_inbox_claim ON webhook_inbox (status, next_attempt_at, received_at);

-- Pre-ingest denials (token mismatch, unresolved secret, unsupported
-- event kind) are audited without any inbox row, so the delivery
-- reference must be nullable.
ALTER TABLE webhook_deliveries ALTER COLUMN inbox_id DROP NOT NULL;
