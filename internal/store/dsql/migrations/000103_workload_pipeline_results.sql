CREATE TABLE IF NOT EXISTS workload_pipeline_results (
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    pipeline_name TEXT NOT NULL CHECK (pipeline_name ~ '^[a-z][a-z0-9_-]{0,39}$'),
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'failed', 'cancelled', 'timed_out', 'skipped')),
    exit_code INTEGER NOT NULL CHECK (exit_code BETWEEN 0 AND 255),
    error_code TEXT CHECK (error_code IS NULL OR error_code ~ '^[A-Za-z0-9_.-]{1,128}$'),
    completed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, workload_run_id, generation, pipeline_name),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id),
    CHECK ((status = 'succeeded' AND exit_code = 0 AND error_code IS NULL)
        OR (status = 'failed' AND exit_code > 0)
        OR (status IN ('cancelled', 'timed_out') AND exit_code > 0)
        OR (status = 'skipped' AND exit_code = 0 AND error_code IS NULL))
);
