-- Restores the 0001 bigint typing (pre-cutover drill data only).
ALTER TABLE leases ALTER COLUMN connection_generation DROP DEFAULT;
ALTER TABLE leases ALTER COLUMN connection_generation TYPE bigint
    USING CASE
        WHEN connection_generation ~ '^[0-9]+$' THEN connection_generation::bigint
        ELSE 1
    END;
ALTER TABLE leases ALTER COLUMN connection_generation SET DEFAULT 1;
