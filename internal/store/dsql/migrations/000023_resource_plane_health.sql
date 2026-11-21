CREATE TABLE IF NOT EXISTS resource_plane_health (
    resource_plane_id TEXT PRIMARY KEY REFERENCES resource_planes(id),
    status TEXT NOT NULL CHECK (status IN ('healthy', 'degraded', 'unhealthy')),
    reason TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
)
