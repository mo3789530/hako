CREATE INDEX IF NOT EXISTS workload_runs_timeout_idx
    ON workload_runs (timeout_at, id)
