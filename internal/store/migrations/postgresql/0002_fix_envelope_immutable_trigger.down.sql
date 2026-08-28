-- Restores the 0001 version of the trigger function. Applying this revert
-- re-introduces the broken bare-name comparison; it exists only so the
-- migration chain stays reversible, not for production use.
CREATE OR REPLACE FUNCTION maestro_envelope_immutable() RETURNS trigger AS $$
BEGIN
    IF (event_id, event_type, event_version, source, project_id, subject,
        occurred_at, correlation_id, causation_id, payload_digest,
        sensitivity, payload) IS DISTINCT FROM
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
