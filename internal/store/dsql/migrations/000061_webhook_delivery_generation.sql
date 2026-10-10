ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS credential_generation BIGINT NOT NULL DEFAULT 0
