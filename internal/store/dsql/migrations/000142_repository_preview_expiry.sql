ALTER TABLE repository_previews
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
