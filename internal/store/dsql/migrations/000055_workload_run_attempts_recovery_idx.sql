CREATE INDEX IF NOT EXISTS workload_run_attempts_recovery_idx
    ON workload_run_attempts (resource_plane_id, state, updated_at)
