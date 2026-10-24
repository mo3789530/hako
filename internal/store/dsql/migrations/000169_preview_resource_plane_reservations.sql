CREATE TABLE IF NOT EXISTS preview_resource_plane_reservations (
    runtime_workspace_id TEXT PRIMARY KEY,
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    runtime_class TEXT NOT NULL,
    cpu_millicores BIGINT NOT NULL CHECK (cpu_millicores >= 0),
    memory_mib BIGINT NOT NULL CHECK (memory_mib >= 0),
    capacity_charged BOOLEAN NOT NULL,
    reserved_at TIMESTAMPTZ NOT NULL
);
