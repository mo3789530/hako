CREATE TABLE IF NOT EXISTS github_actions_runner_checks (
    integration_id TEXT NOT NULL,
    workflow_job_id BIGINT NOT NULL CHECK (workflow_job_id > 0),
    run_attempt BIGINT NOT NULL CHECK (run_attempt > 0),
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    repository_id BIGINT NOT NULL CHECK (repository_id > 0),
    workflow_run_id BIGINT NOT NULL CHECK (workflow_run_id > 0),
    commit_sha TEXT NOT NULL CHECK (length(commit_sha) IN (40, 64)),
    check_name TEXT NOT NULL CHECK (length(check_name) BETWEEN 1 AND 100),
    external_id TEXT NOT NULL CHECK (length(external_id) BETWEEN 1 AND 255),
    check_run_id BIGINT CHECK (check_run_id IS NULL OR check_run_id > 0),
    status TEXT NOT NULL CHECK (status IN ('queued', 'in_progress', 'completed')),
    conclusion TEXT CHECK (conclusion IN ('success', 'failure', 'neutral', 'cancelled', 'timed_out', 'action_required', 'stale', 'skipped')),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    output_title TEXT NOT NULL CHECK (length(output_title) BETWEEN 1 AND 255),
    output_summary TEXT NOT NULL CHECK (length(output_summary) BETWEEN 1 AND 4096),
    revision BIGINT NOT NULL CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (integration_id, workflow_job_id, run_attempt),
    FOREIGN KEY (integration_id, workflow_job_id, run_attempt)
        REFERENCES github_actions_runner_jobs(integration_id, workflow_job_id, run_attempt),
    CHECK ((status = 'completed' AND conclusion IS NOT NULL AND completed_at IS NOT NULL AND completed_at >= started_at)
        OR (status <> 'completed' AND conclusion IS NULL AND completed_at IS NULL))
);
