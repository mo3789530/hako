ALTER TABLE tenant_github_repositories
    ADD COLUMN IF NOT EXISTS actions_runner_workflow_policy JSONB,
    ADD CONSTRAINT tenant_github_repositories_runner_policy_object
    CHECK (actions_runner_workflow_policy IS NULL OR jsonb_typeof(actions_runner_workflow_policy) = 'object');
