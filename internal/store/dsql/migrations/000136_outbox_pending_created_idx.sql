CREATE INDEX IF NOT EXISTS outbox_pending_created_idx
    ON outbox_events (created_at, id)
    WHERE published_at IS NULL;
