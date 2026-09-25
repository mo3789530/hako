CREATE TABLE IF NOT EXISTS volumes (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    mount_path TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, name)
);
