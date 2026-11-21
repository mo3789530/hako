ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS preview_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS preview_enabled_at TIMESTAMPTZ;
