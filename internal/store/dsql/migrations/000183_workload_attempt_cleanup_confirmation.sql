ALTER TABLE workload_run_attempts
    ADD COLUMN cleanup_confirmed_at TIMESTAMPTZ;
