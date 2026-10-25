CREATE TABLE workload_agent_proposal_verifications (
    tenant_id TEXT NOT NULL,
    agent_run_id TEXT NOT NULL,
    agent_attempt_id TEXT NOT NULL REFERENCES workload_run_attempts(id),
    proposal_artifact_id TEXT NOT NULL REFERENCES workload_artifacts(id),
    proposal_sha256 TEXT NOT NULL CHECK (proposal_sha256 ~ '^[0-9a-f]{64}$'),
    base_commit_sha TEXT NOT NULL CHECK (base_commit_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
    verifier_run_id TEXT NOT NULL,
    verifier_attempt_id TEXT NOT NULL REFERENCES workload_run_attempts(id),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, agent_run_id, agent_attempt_id),
    UNIQUE (tenant_id, verifier_run_id),
    FOREIGN KEY (tenant_id, agent_run_id) REFERENCES workload_runs(tenant_id, id),
    FOREIGN KEY (tenant_id, verifier_run_id) REFERENCES workload_runs(tenant_id, id)
);
