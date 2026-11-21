ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS agent_model_access_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS agent_model_access_enabled_at TIMESTAMPTZ;
