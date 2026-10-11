ALTER TABLE tenant_github_installations
    ADD COLUMN IF NOT EXISTS github_app_id BIGINT;
