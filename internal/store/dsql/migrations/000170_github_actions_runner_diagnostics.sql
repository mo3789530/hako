ALTER TABLE github_actions_runner_jobs
    ADD COLUMN diagnostics_upload_token_sha256 TEXT
        CHECK (diagnostics_upload_token_sha256 IS NULL OR diagnostics_upload_token_sha256 ~ '^[a-f0-9]{64}$');
