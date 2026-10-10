ALTER TABLE github_credential_bindings
    ADD COLUMN IF NOT EXISTS webhook_secret_reader_role_arn TEXT,
    ADD COLUMN IF NOT EXISTS webhook_secret_version_id TEXT,
    ADD COLUMN IF NOT EXISTS previous_webhook_secret_version_id TEXT,
    ADD COLUMN IF NOT EXISTS previous_webhook_secret_valid_until TIMESTAMPTZ
