ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS actions_runner_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS actions_runner_enabled_at TIMESTAMPTZ;
