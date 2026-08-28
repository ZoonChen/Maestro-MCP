-- Reverts 0001_m1_baseline by dropping the M1 schema.
--
-- Rollback semantics (TECH-DATA-001 section 13 / ADR-002): reverting is only
-- valid BEFORE cutover — after cutover, recovery is PostgreSQL PITR or
-- forward-fix, never a return to dual-write with SQLite. The drop order is
-- irrelevant because PostgreSQL does not require it, but dropping the whole
-- non-system schema guarantees no orphan objects survive.
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
