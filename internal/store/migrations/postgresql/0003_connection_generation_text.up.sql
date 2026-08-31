-- Fixes the 0001 drift: the frozen runner.yaml types
-- connection_generation as a UUID string (the daemon mints one UUIDv7
-- per connection), but the baseline column was bigint. Existing numeric
-- generations (test/seed data only; no production cutover has happened)
-- convert to text losslessly for compatibility.
ALTER TABLE leases ALTER COLUMN connection_generation TYPE text
    USING connection_generation::text;
ALTER TABLE leases ALTER COLUMN connection_generation SET DEFAULT '';
