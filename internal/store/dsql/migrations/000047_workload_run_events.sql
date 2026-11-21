CREATE TABLE IF NOT EXISTS workload_run_events (
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    event_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workload_run_id, sequence),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id)
)
