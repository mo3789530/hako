CREATE TABLE IF NOT EXISTS resource_bindings (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    kind TEXT NOT NULL,
    external_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, kind)
);
