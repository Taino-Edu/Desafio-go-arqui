-- Reverte 000002_outbox_sequence.
CREATE OR REPLACE FUNCTION outbox_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.event_id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version,
        NEW.correlation_id, NEW.causation_id, NEW.payload, NEW.occurred_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.event_id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version,
        OLD.correlation_id, OLD.causation_id, OLD.payload, OLD.occurred_at, OLD.created_at) THEN
        RAISE EXCEPTION 'outbox_events: event identity and payload are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox_events: published_at cannot change once set'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

DROP INDEX IF EXISTS outbox_unpublished_aggregate_seq_idx;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_seq_uq;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS seq;
