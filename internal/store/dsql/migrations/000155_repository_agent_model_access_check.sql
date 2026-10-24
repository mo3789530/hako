ALTER TABLE tenant_github_repositories
    ADD CONSTRAINT tenant_github_repositories_agent_model_access_check
    CHECK ((agent_model_access_enabled AND agent_model_access_enabled_at IS NOT NULL) OR
           (NOT agent_model_access_enabled AND agent_model_access_enabled_at IS NULL));
