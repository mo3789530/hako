CREATE TABLE IF NOT EXISTS tenant_quota_limits (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
    workspace_limit SMALLINT NOT NULL CHECK (workspace_limit > 0 AND workspace_limit <= 10),
    concurrent_workload_run_limit SMALLINT NOT NULL CHECK (concurrent_workload_run_limit > 0 AND concurrent_workload_run_limit <= 5),
    revision BIGINT NOT NULL CHECK (revision > 0),
    updated_by TEXT REFERENCES users(id),
    updated_at TIMESTAMPTZ NOT NULL
)
