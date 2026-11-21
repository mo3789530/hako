ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS active_connection_lease_id TEXT,
    ADD COLUMN IF NOT EXISTS active_connection_expires_at TIMESTAMPTZ;
