ALTER TABLE repository_preview_runtime_allocations
    ADD COLUMN IF NOT EXISTS github_integration_id TEXT,
    ADD COLUMN IF NOT EXISTS github_config_revision BIGINT,
    ADD COLUMN IF NOT EXISTS github_credential_generation BIGINT
