CREATE TABLE IF NOT EXISTS operation_events (
    operation_id TEXT NOT NULL REFERENCES operations(id),
    sequence BIGINT NOT NULL,
    event_type TEXT NOT NULL,
    payload_json TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (operation_id, sequence)
);
