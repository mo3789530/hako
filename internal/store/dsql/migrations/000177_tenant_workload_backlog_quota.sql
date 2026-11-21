ALTER TABLE tenant_quota_limits
    ADD COLUMN IF NOT EXISTS queued_workload_run_limit SMALLINT NOT NULL DEFAULT 5
        CHECK (queued_workload_run_limit > 0 AND queued_workload_run_limit <= 5);
