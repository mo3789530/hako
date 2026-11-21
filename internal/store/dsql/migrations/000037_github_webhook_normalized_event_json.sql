ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS normalized_event_json TEXT NOT NULL DEFAULT '{}'
