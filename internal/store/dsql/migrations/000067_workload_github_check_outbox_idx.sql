CREATE INDEX IF NOT EXISTS workload_github_check_outbox_idx
    ON outbox_events (created_at, id)
    WHERE published_at IS NULL AND event_type = 'workload_run.github_check_requested';
