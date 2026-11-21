ALTER TABLE github_actions_runner_jobs
    DROP CONSTRAINT github_actions_runner_jobs_claim_lease_check,
    ADD CONSTRAINT github_actions_runner_jobs_claim_lease_check CHECK ((state IN ('claimed', 'starting') AND claim_id IS NOT NULL AND lease_expires_at IS NOT NULL) OR (state NOT IN ('claimed', 'starting') AND claim_id IS NULL AND lease_expires_at IS NULL));
