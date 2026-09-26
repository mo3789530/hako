CREATE TABLE IF NOT EXISTS platform_audit_events (
    id TEXT PRIMARY KEY,
    actor_user_id TEXT NOT NULL REFERENCES users(id),
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    details_json TEXT NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ NOT NULL
)
