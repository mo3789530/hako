ALTER TABLE tenant_quota_limits
    ADD CONSTRAINT tenant_quota_queue_within_concurrency
        CHECK (queued_workload_run_limit <= concurrent_workload_run_limit);
