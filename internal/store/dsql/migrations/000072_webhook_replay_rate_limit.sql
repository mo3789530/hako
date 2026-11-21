ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS replay_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS replay_window_started_at TIMESTAMPTZ
