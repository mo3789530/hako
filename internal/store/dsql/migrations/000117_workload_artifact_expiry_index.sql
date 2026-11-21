CREATE INDEX IF NOT EXISTS workload_artifacts_expiry_idx
    ON workload_artifacts (expires_at, id)
    WHERE expires_at IS NOT NULL;
