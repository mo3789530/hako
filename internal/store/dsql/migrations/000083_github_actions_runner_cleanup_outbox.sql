CREATE TABLE IF NOT EXISTS github_actions_runner_cleanup_outbox (
    integration_id TEXT NOT NULL,
    workflow_job_id BIGINT NOT NULL CHECK (workflow_job_id > 0),
    run_attempt BIGINT NOT NULL CHECK (run_attempt > 0),
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    runner_id BIGINT CHECK (runner_id IS NULL OR runner_id > 0),
    reason TEXT NOT NULL CHECK (reason IN ('completed', 'failed', 'cancelled')),
    state TEXT NOT NULL CHECK (state IN ('pending', 'claimed', 'completed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at TIMESTAMPTZ NOT NULL,
    claim_id TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '' CHECK (length(last_error) <= 256),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (integration_id, workflow_job_id, run_attempt),
    FOREIGN KEY (integration_id, workflow_job_id, run_attempt)
        REFERENCES github_actions_runner_jobs(integration_id, workflow_job_id, run_attempt),
    CHECK ((state = 'claimed' AND claim_id IS NOT NULL AND lease_expires_at IS NOT NULL) OR
           (state <> 'claimed' AND claim_id IS NULL AND lease_expires_at IS NULL))
);
