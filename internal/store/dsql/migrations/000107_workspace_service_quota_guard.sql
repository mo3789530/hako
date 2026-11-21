CREATE TABLE IF NOT EXISTS workspace_service_quota_guards (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
    revision BIGINT NOT NULL CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL
)
