ALTER TABLE github_actions_runner_jobs
    ADD COLUMN claim_id TEXT,
    ADD COLUMN lease_expires_at TIMESTAMPTZ,
    ADD COLUMN claim_attempts INTEGER NOT NULL DEFAULT 0 CHECK (claim_attempts >= 0),
    DROP CONSTRAINT github_actions_runner_jobs_state_check,
    ADD CONSTRAINT github_actions_runner_jobs_state_check CHECK (state IN ('admitted', 'claimed', 'starting', 'assigned', 'running', 'completed', 'failed', 'cancelled')),
    ADD CONSTRAINT github_actions_runner_jobs_claim_lease_check CHECK ((state = 'claimed' AND claim_id IS NOT NULL AND lease_expires_at IS NOT NULL) OR (state <> 'claimed' AND claim_id IS NULL AND lease_expires_at IS NULL));
