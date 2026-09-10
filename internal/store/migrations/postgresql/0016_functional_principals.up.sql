-- J1 functional principals (task brief J1 / G-α / UI-4; W4.5 wave).
--
-- Authority: docs/specs/rbac/permissions.yaml `functional_approvers`
-- (frozen matrix) and plans/prep/pilot/SOLUTION-BLUEPRINT.md §1.3/§4.2.
-- The frozen matrix grants waiver.approve / waiver.approve.security /
-- waiver.approve.quality / audit.export etc. to functional approvers
-- (security_owner, qa_owner), but until now no principal could HOLD a
-- functional role: memberships carry project roles only. This table is
-- the explicit functional-binding configuration store — one row per
-- (user, function) grant with a validity window, a terminal revocation
-- bit and the authorization-source reference text (the 授权书 citation
-- of blueprint §4.2; enforcing that a resolvable deed exists is the
-- registered J2a follow-up once the asset ledger lands).
--
-- The table sits BESIDE memberships, never inside it: a functional
-- grant confers only the frozen functional permissions (it never
-- stacks project-role permissions), and the frozen conditions on the
-- waiver actions still require an active project membership — the
-- resolver keeps deriving that map from memberships alone.

CREATE TABLE functional_principals (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id),
    function    text NOT NULL
        CHECK (function IN ('security_owner', 'qa_owner', 'operations_owner',
                            'product_owner', 'technical_lead')),
    valid_from  timestamptz NOT NULL DEFAULT now(),
    valid_to    timestamptz,
    revoked_at  timestamptz,
    source_ref  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    -- Revocation is terminal and can only follow the grant itself.
    CHECK (revoked_at IS NULL OR revoked_at >= valid_from)
);

-- The resolver's active-grant lookup is (user_id) scoped; keep it index
-- driven instead of scanning the table per request.
CREATE INDEX idx_functional_principals_user ON functional_principals (user_id);

-- One ACTIVE grant per (user, function): a second grant for the same
-- function must revoke-or-expire the first, never accumulate history
-- that the resolver would have to arbitrate.
CREATE UNIQUE INDEX uniq_functional_principals_active
    ON functional_principals (user_id, function)
    WHERE revoked_at IS NULL;
