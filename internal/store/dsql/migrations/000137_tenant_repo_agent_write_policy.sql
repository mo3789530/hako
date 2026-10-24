ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS agent_write_policy JSONB,
    ADD CONSTRAINT tenant_github_repositories_agent_write_policy_object
    CHECK (agent_write_policy IS NULL OR jsonb_typeof(agent_write_policy) = 'object');
