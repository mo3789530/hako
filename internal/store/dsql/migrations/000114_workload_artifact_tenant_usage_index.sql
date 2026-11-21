CREATE INDEX IF NOT EXISTS workload_artifacts_tenant_usage_idx
    ON workload_artifacts (tenant_id, size_bytes);
