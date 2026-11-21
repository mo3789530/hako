ALTER TABLE github_actions_runner_checks
    ADD COLUMN diagnostics_sha256 TEXT
        CHECK (diagnostics_sha256 IS NULL OR diagnostics_sha256 ~ '^[a-f0-9]{64}$'),
    ADD COLUMN diagnostics_summary TEXT NOT NULL DEFAULT ''
        CHECK (octet_length(diagnostics_summary) <= 3072),
    ADD COLUMN diagnostics_uploaded_at TIMESTAMPTZ;
