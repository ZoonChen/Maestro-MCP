-- J5 platform grants (task brief J5 / CR-P5a-1; P5 wave).
--
-- Authority: docs/specs/rbac/permissions.yaml `roles.platform_admin`
-- (frozen matrix) and plans/prep/pilot/SOLUTION-BLUEPRINT.md §1.3/§4.2.
-- The frozen matrix grants the platform-level permission strings
-- (platform.configure, oidc.configure, gitlab_instance.configure,
-- company_policy.manage, security.emergency_stop, audit.export,
-- pilot.read, pilot.write) to the platform_admin role, but until now no
-- principal could HOLD that role: memberships.role carries project
-- roles only (0001 CHECK — platform_admin is deliberately barred from
-- project membership) and the J1 functional plane covers the
-- functional_approvers strings only. This table is the explicit
-- platform-binding configuration store — one row per (user, role)
-- grant with a validity window, a terminal revocation bit and the
-- authorization-source reference text (the 授权书 citation of
-- blueprint §4.2, same discipline as functional_principals).
--
-- The table sits BESIDE memberships and functional_principals: a
-- platform grant confers only the frozen platform-role permissions (it
-- never stacks project permissions), needs no project membership, and
-- the resolver refreshes it per request so expiry and revocation
-- propagate on the next call.

CREATE TABLE platform_grants (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id),
    role        text NOT NULL
        CHECK (role IN ('platform_admin')),
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
CREATE INDEX idx_platform_grants_user ON platform_grants (user_id);

-- One ACTIVE grant per (user, role): a second grant of the same
-- platform role must revoke-or-expire the first, never accumulate
-- history that the resolver would have to arbitrate.
CREATE UNIQUE INDEX uniq_platform_grants_active
    ON platform_grants (user_id, role)
    WHERE revoked_at IS NULL;
