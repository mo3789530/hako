CREATE INDEX IF NOT EXISTS repository_preview_sessions_expiry_idx
    ON repository_preview_sessions (expires_at, id);
