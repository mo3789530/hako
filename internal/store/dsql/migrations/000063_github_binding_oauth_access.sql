ALTER TABLE github_credential_bindings
    ADD COLUMN IF NOT EXISTS oauth_reader_role_arn TEXT,
    ADD COLUMN IF NOT EXISTS oauth_secret_version_id TEXT
