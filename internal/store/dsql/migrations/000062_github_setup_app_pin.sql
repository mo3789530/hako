ALTER TABLE github_installation_setup_states
    ADD COLUMN IF NOT EXISTS integration_id TEXT,
    ADD COLUMN IF NOT EXISTS app_registration_id TEXT,
    ADD COLUMN IF NOT EXISTS github_app_id BIGINT,
    ADD COLUMN IF NOT EXISTS config_revision BIGINT,
    ADD COLUMN IF NOT EXISTS credential_generation BIGINT
