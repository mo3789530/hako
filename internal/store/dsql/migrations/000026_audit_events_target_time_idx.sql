CREATE INDEX IF NOT EXISTS audit_events_target_time_idx
    ON audit_events (target_type, target_id, occurred_at DESC, id DESC)
