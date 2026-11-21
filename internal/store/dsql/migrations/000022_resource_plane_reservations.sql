CREATE TABLE IF NOT EXISTS resource_plane_reservations (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    reserved_at TIMESTAMPTZ NOT NULL
)
