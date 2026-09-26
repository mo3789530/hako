ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS installation_id BIGINT;
