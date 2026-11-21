ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS event_schema_version INTEGER NOT NULL DEFAULT 0
