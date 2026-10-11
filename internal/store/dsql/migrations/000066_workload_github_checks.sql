CREATE TABLE IF NOT EXISTS workload_github_checks (
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    github_installation_id BIGINT NOT NULL CHECK (github_installation_id > 0),
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    commit_sha TEXT NOT NULL CHECK (length(commit_sha) IN (40, 64)),
    check_name TEXT NOT NULL CHECK (length(check_name) BETWEEN 1 AND 100),
    external_id TEXT NOT NULL CHECK (length(external_id) BETWEEN 1 AND 255),
    check_run_id BIGINT CHECK (check_run_id > 0),
    status TEXT NOT NULL CHECK (status IN ('queued', 'in_progress', 'completed')),
    conclusion TEXT CHECK (conclusion IN ('success', 'failure', 'neutral', 'cancelled', 'timed_out', 'action_required', 'stale', 'skipped')),
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    output_title TEXT NOT NULL CHECK (length(output_title) BETWEEN 1 AND 255),
    output_summary TEXT NOT NULL CHECK (length(output_summary) BETWEEN 1 AND 4096),
    revision BIGINT NOT NULL CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, workload_run_id),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id),
    CHECK ((status = 'completed' AND conclusion IS NOT NULL AND completed_at IS NOT NULL AND completed_at >= started_at)
        OR (status <> 'completed' AND conclusion IS NULL AND completed_at IS NULL))
);
