-- 000002_outbox_sequence: ordem de publicação por agregado.
--
-- seq é atribuído pelo banco no INSERT. Os eventos de uma carteira são
-- gravados com a carteira travada (FOR UPDATE), então seq cresce na mesma
-- ordem das versões da carteira, sem depender do relógio de cada instância.
-- O publicador só pega o evento mais antigo ainda não publicado de cada
-- agregado, publicando cada agregado em ordem.
ALTER TABLE outbox_events
    ADD COLUMN seq BIGINT GENERATED ALWAYS AS IDENTITY;

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_seq_uq UNIQUE (seq);

-- busca do "próximo evento pendente de cada agregado"
CREATE INDEX outbox_unpublished_aggregate_seq_idx
    ON outbox_events (aggregate_id, seq)
    WHERE published_at IS NULL;

-- seq também é imutável
CREATE OR REPLACE FUNCTION outbox_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.event_id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version,
        NEW.correlation_id, NEW.causation_id, NEW.payload, NEW.occurred_at, NEW.created_at, NEW.seq)
       IS DISTINCT FROM
       (OLD.event_id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version,
        OLD.correlation_id, OLD.causation_id, OLD.payload, OLD.occurred_at, OLD.created_at, OLD.seq) THEN
        RAISE EXCEPTION 'outbox_events: event identity and payload are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox_events: published_at cannot change once set'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
