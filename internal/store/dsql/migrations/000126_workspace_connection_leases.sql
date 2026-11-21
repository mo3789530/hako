CREATE TABLE IF NOT EXISTS workspace_connection_leases (
    lease_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    microvm_id TEXT NOT NULL,
    allocation_generation BIGINT NOT NULL CHECK (allocation_generation > 0),
    expires_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    renewed_at TIMESTAMPTZ NOT NULL,
    CHECK (released_at IS NULL OR released_at >= created_at)
);
