-- J5 platform grants: drop the platform-binding table. The
-- platform-level permission strings return to the pre-J5 fail-closed
-- state (no principal holds platform_admin; CR-P5a-1 reappears).

DROP INDEX IF EXISTS uniq_platform_grants_active;
DROP INDEX IF EXISTS idx_platform_grants_user;
DROP TABLE IF EXISTS platform_grants;
