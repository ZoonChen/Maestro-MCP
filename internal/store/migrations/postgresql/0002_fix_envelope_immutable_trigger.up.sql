-- Fixes the envelope-immutability trigger function from 0001: the original
-- body compared bare column names (event_id, ...) against NEW.* values, and
-- PL/pgSQL resolves bare names as variables first, so every UPDATE on
-- outbox_events/inbox_events failed with "column ... does not exist".
-- The corrected body compares OLD.* (the stored immutable envelope) with
-- NEW.* explicitly; semantics are unchanged (ADR-002 dispatch bookkeeping
-- may transition, envelope columns may not).
CREATE OR REPLACE FUNCTION maestro_envelope_immutable() RETURNS trigger AS $$
BEGIN
    IF (OLD.event_id, OLD.event_type, OLD.event_version, OLD.source,
        OLD.project_id, OLD.subject, OLD.occurred_at, OLD.correlation_id,
        OLD.causation_id, OLD.payload_digest, OLD.sensitivity, OLD.payload)
        IS DISTINCT FROM
       (NEW.event_id, NEW.event_type, NEW.event_version, NEW.source,
        NEW.project_id, NEW.subject, NEW.occurred_at, NEW.correlation_id,
        NEW.causation_id, NEW.payload_digest, NEW.sensitivity, NEW.payload)
    THEN
        RAISE EXCEPTION 'EVENT_ENVELOPE_IMMUTABLE'
            USING ERRCODE = 'raise_exception',
                  HINT = 'dispatch state may change, the durable envelope may not';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
