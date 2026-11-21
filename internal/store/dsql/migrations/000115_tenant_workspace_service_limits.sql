ALTER TABLE tenant_quota_limits
    ADD COLUMN IF NOT EXISTS workspace_service_memory_bytes_limit BIGINT NOT NULL DEFAULT 2147483648
        CHECK (workspace_service_memory_bytes_limit > 0 AND workspace_service_memory_bytes_limit <= 2147483648),
    ADD COLUMN IF NOT EXISTS workspace_service_milli_cpu_limit BIGINT NOT NULL DEFAULT 1500
        CHECK (workspace_service_milli_cpu_limit > 0 AND workspace_service_milli_cpu_limit <= 1500);
