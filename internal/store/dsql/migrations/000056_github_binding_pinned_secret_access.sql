ALTER TABLE github_credential_bindings
    ADD COLUMN IF NOT EXISTS private_key_reader_role_arn TEXT,
    ADD COLUMN IF NOT EXISTS private_key_secret_version_id TEXT;

