CREATE TABLE IF NOT EXISTS workspace_service_quota_reservations (
    service_id TEXT PRIMARY KEY REFERENCES services(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    memory_bytes BIGINT NOT NULL CHECK (memory_bytes > 0),
    milli_cpu BIGINT NOT NULL CHECK (milli_cpu > 0),
    reserved_at TIMESTAMPTZ NOT NULL
)
