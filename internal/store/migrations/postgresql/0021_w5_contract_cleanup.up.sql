-- ---------------------------------------------------------------------------
-- W5 contract cleanup (ART-retrospective-001 seven-item slice)
--
-- 1. Multi-sign asset release: release-note class assets require the
--    product_owner AND technical_lead signoffs before the approved flip
--    (W5-2). Existing release-note rows are backfilled with the frozen
--    default pair; every other type keeps the single-approve contract
--    unless the registration declares additional required roles.
-- 2. Per-role signoff ledger for the multi-sign gate.
-- 3. decomposition_propose idempotency keys move from the accidental
--    GLOBAL namespace to the per-project namespace (W5-3): two projects
--    minting the same caller key must never replay each other's decided
--    proposals.
-- ---------------------------------------------------------------------------

ALTER TABLE assets ADD COLUMN required_approver_roles jsonb NOT NULL DEFAULT '[]'::jsonb;

UPDATE assets SET required_approver_roles = '["product_owner","technical_lead"]'::jsonb
WHERE asset_type = 'release-note';

CREATE TABLE asset_approvals (
    asset_id          text NOT NULL,
    version           integer NOT NULL CHECK (version > 0),
    approver_role     text NOT NULL CHECK (approver_role IN (
        'security_owner', 'qa_owner', 'operations_owner', 'product_owner', 'technical_lead')),
    approver_principal text NOT NULL,
    decided_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (asset_id, version, approver_role),
    FOREIGN KEY (asset_id, version) REFERENCES assets (asset_id, version) ON DELETE CASCADE
);

DROP INDEX uniq_decomposition_proposals_key;
CREATE UNIQUE INDEX uniq_decomposition_proposals_project_key
    ON decomposition_proposals (project_id, idempotency_key);
