-- J1 functional principals: drop the functional-binding table. Waiver
-- approval returns to the pre-J1 fail-closed state (no principal holds
-- a functional role).

DROP INDEX IF EXISTS uniq_functional_principals_active;
DROP INDEX IF EXISTS idx_functional_principals_user;
DROP TABLE IF EXISTS functional_principals;
