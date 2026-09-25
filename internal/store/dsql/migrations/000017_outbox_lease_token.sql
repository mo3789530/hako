ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS lease_token TEXT
