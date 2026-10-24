CREATE TABLE IF NOT EXISTS workload_quota_slots (
    tenant_id TEXT NOT NULL,
    slot INTEGER NOT NULL CHECK (slot > 0),
    workload_run_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, slot),
    UNIQUE (tenant_id, workload_run_id),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id)
);
