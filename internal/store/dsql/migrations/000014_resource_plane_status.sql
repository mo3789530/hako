CREATE TABLE IF NOT EXISTS resource_plane_status (
    resource_plane_id TEXT PRIMARY KEY REFERENCES resource_planes(id),
    status TEXT NOT NULL CHECK (status IN ('active', 'draining', 'disabled')),
    updated_at TIMESTAMPTZ NOT NULL
);
