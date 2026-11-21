ALTER TABLE tenant_github_installations
    ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ
