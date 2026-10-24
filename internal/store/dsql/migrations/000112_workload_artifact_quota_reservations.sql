CREATE TABLE IF NOT EXISTS workload_artifact_quota_reservations (
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    reserved_bytes BIGINT NOT NULL CHECK (reserved_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, workload_run_id),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id)
);
