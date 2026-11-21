CREATE INDEX IF NOT EXISTS github_webhook_payload_retention_idx
    ON github_webhook_deliveries (payload_expires_at, integration_id, delivery_id)
    WHERE payload_purged_at IS NULL
