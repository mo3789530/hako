ALTER TABLE workload_runs
    ADD COLUMN IF NOT EXISTS github_integration_id TEXT,
    ADD COLUMN IF NOT EXISTS github_config_revision BIGINT,
    ADD COLUMN IF NOT EXISTS github_credential_generation BIGINT,
    ADD CONSTRAINT workload_runs_github_integration_fk FOREIGN KEY (tenant_id, github_integration_id) REFERENCES tenant_github_integrations(tenant_id, id),
    ADD CONSTRAINT workload_runs_github_identity_check CHECK (
        (github_integration_id IS NULL AND github_config_revision IS NULL AND github_credential_generation IS NULL)
        OR (github_integration_id IS NOT NULL AND github_config_revision > 0 AND github_credential_generation > 0)
    )
