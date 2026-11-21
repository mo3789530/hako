ALTER TABLE tenant_quota_limits
    ADD COLUMN IF NOT EXISTS workspace_compute_milli_cpu_limit BIGINT NOT NULL DEFAULT 0
        CHECK (workspace_compute_milli_cpu_limit >= 0 AND workspace_compute_milli_cpu_limit <= 80000),
    ADD COLUMN IF NOT EXISTS workspace_compute_memory_mib_limit BIGINT NOT NULL DEFAULT 0
        CHECK (workspace_compute_memory_mib_limit >= 0 AND workspace_compute_memory_mib_limit <= 163840)
