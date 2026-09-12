-- Best-effort down for the W5 contract cleanup. The global idempotency
-- index can only be recreated when no key repeats across projects; the
-- signoff ledger and the required-roles column drop with their data.

DROP INDEX IF EXISTS uniq_decomposition_proposals_project_key;
CREATE UNIQUE INDEX uniq_decomposition_proposals_key ON decomposition_proposals (idempotency_key);

DROP TABLE IF EXISTS asset_approvals;

ALTER TABLE assets DROP COLUMN IF EXISTS required_approver_roles;
