CREATE TABLE IF NOT EXISTS resource_plane_capacities (
    resource_plane_id TEXT PRIMARY KEY REFERENCES resource_planes(id),
    max_workspaces INTEGER NOT NULL CHECK (max_workspaces > 0),
    reserved_workspaces INTEGER NOT NULL CHECK (reserved_workspaces >= 0),
    updated_at TIMESTAMPTZ NOT NULL
)
