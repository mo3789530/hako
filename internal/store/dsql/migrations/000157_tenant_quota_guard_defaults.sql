ALTER TABLE tenant_quota_limits
    ALTER COLUMN workspace_limit SET DEFAULT 10,
    ALTER COLUMN concurrent_workload_run_limit SET DEFAULT 5,
    ALTER COLUMN preview_run_limit SET DEFAULT 2,
    ALTER COLUMN preview_lifetime_seconds SET DEFAULT 86400,
    ALTER COLUMN artifact_bytes_limit SET DEFAULT 10737418240,
    ALTER COLUMN workspace_service_memory_bytes_limit SET DEFAULT 2147483648,
    ALTER COLUMN workspace_service_milli_cpu_limit SET DEFAULT 1500;
