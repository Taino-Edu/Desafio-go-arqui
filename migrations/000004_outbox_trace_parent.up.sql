-- 000004_outbox_trace_parent: contexto de trace do evento.
--
-- O evento é gravado no mesmo commit da operação, com o traceparent (W3C)
-- da requisição ou mensagem que o originou. O publicador, mais tarde e
-- talvez em outra instância, continua o MESMO trace ao publicar, e envia o
-- traceparent como atributo da mensagem para o próximo consumidor.
ALTER TABLE outbox_events
    ADD COLUMN trace_parent TEXT
        CHECK (trace_parent IS NULL OR trace_parent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$');

-- trace_parent também é imutável (faz parte do registro do evento)
CREATE OR REPLACE FUNCTION outbox_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.event_id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version,
        NEW.correlation_id, NEW.causation_id, NEW.payload, NEW.occurred_at, NEW.created_at, NEW.seq,
        NEW.trace_parent)
       IS DISTINCT FROM
       (OLD.event_id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version,
        OLD.correlation_id, OLD.causation_id, OLD.payload, OLD.occurred_at, OLD.created_at, OLD.seq,
        OLD.trace_parent) THEN
        RAISE EXCEPTION 'outbox_events: event identity and payload are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox_events: published_at cannot change once set'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
