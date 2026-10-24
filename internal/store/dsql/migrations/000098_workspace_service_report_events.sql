CREATE TABLE IF NOT EXISTS workspace_service_report_events (
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    report_id TEXT NOT NULL,
    service_id TEXT NOT NULL REFERENCES services(id),
    revision BIGINT NOT NULL CHECK (revision > 0),
    payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (resource_plane_id, report_id)
)
