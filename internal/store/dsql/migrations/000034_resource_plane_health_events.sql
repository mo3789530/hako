CREATE TABLE IF NOT EXISTS resource_plane_health_events (
    id TEXT PRIMARY KEY,
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    status TEXT NOT NULL CHECK (status IN ('healthy', 'degraded', 'unhealthy')),
    report_source TEXT NOT NULL CHECK (report_source IN ('operator', 'automated')),
    reason TEXT NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL
)
