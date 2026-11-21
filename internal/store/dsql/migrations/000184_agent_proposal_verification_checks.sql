CREATE TABLE workload_agent_proposal_verification_checks (
    tenant_id TEXT NOT NULL,
    verifier_run_id TEXT NOT NULL,
    verifier_attempt_id TEXT NOT NULL REFERENCES workload_run_attempts(id),
    generation BIGINT NOT NULL CHECK (generation > 0),
    pipeline_name TEXT NOT NULL CHECK (pipeline_name ~ '^[a-z][a-z0-9_-]{0,39}$'),
    proposal_sha256 TEXT NOT NULL CHECK (proposal_sha256 ~ '^[0-9a-f]{64}$'),
    base_commit_sha TEXT NOT NULL CHECK (base_commit_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'skipped')),
    exit_code INTEGER NOT NULL CHECK (exit_code BETWEEN 0 AND 255),
    error_code TEXT,
    completed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, verifier_run_id, generation, pipeline_name),
    FOREIGN KEY (tenant_id, verifier_run_id) REFERENCES workload_agent_proposal_verifications(tenant_id, verifier_run_id),
    FOREIGN KEY (tenant_id, verifier_run_id, generation, pipeline_name)
        REFERENCES workload_pipeline_results(tenant_id, workload_run_id, generation, pipeline_name),
    CHECK ((status = 'succeeded' AND exit_code = 0 AND error_code IS NULL)
        OR (status = 'failed' AND exit_code > 0)
        OR (status IN ('cancelled', 'timed_out') AND exit_code > 0)
        OR (status = 'skipped' AND exit_code = 0 AND error_code IS NULL))
);
