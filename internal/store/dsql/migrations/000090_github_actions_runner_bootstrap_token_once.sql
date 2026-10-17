ALTER TABLE github_actions_runner_jobs
    ADD COLUMN IF NOT EXISTS registration_token_issued_at TIMESTAMPTZ;
