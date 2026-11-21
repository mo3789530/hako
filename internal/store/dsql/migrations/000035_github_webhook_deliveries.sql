CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
    delivery_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    action TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    payload_sha256 TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    processing_status TEXT NOT NULL CHECK (processing_status IN ('received', 'processing', 'processed', 'failed'))
)
