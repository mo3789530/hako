ALTER TABLE repository_previews
    ADD COLUMN IF NOT EXISTS cleanup_claim_id TEXT,
    ADD COLUMN IF NOT EXISTS cleanup_lease_expires_at TIMESTAMPTZ;
