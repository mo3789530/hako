CREATE INDEX IF NOT EXISTS repository_preview_runtime_app_pin_idx
    ON repository_preview_runtime_allocations(tenant_id, github_integration_id, github_config_revision, github_credential_generation)
    WHERE github_integration_id IS NOT NULL
