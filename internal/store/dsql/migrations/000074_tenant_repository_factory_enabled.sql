ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS factory_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS factory_enabled_at TIMESTAMPTZ;
