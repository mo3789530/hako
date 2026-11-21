ALTER TABLE repository_preview_runtime_allocations
    ADD CONSTRAINT repository_preview_runtime_app_pin_complete
    CHECK (
        (github_integration_id IS NULL AND github_config_revision IS NULL AND github_credential_generation IS NULL)
        OR
        (github_integration_id IS NOT NULL AND github_config_revision IS NOT NULL AND github_config_revision > 0
            AND github_credential_generation IS NOT NULL AND github_credential_generation > 0)
    )
