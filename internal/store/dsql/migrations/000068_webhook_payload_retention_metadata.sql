ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS actor_login TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS payload_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS payload_purged_at TIMESTAMPTZ
