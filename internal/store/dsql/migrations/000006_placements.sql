CREATE TABLE IF NOT EXISTS placements (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    placed_at TIMESTAMPTZ NOT NULL
);
