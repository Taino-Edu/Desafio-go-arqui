CREATE INDEX outbox_unpublished_idx
    ON outbox_events (next_attempt_at)
    WHERE published_at IS NULL;

DROP INDEX IF EXISTS outbox_pending_seq_idx;
