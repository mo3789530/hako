ALTER TABLE github_actions_runner_jobs
    ADD COLUMN IF NOT EXISTS provenance_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS provenance_policy_sha256 TEXT,
    ADD CONSTRAINT github_actions_runner_provenance_policy_hash
    CHECK (provenance_policy_sha256 IS NULL OR provenance_policy_sha256 ~ '^[0-9a-f]{64}$');
