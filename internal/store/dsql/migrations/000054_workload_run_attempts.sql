CREATE TABLE IF NOT EXISTS workload_run_attempts (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    workload_run_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    client_token TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('dispatch_pending', 'accepted', 'running', 'succeeded', 'failed', 'cancelled', 'timed_out', 'cleanup_pending', 'cleanup_confirmed', 'cleanup_failed')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, workload_run_id, generation),
    UNIQUE (tenant_id, workload_run_id, attempt_number),
    FOREIGN KEY (tenant_id, workload_run_id) REFERENCES workload_runs(tenant_id, id)
)
