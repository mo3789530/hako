CREATE TABLE IF NOT EXISTS tenant_placement_policies (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
    allowed_regions_json TEXT NOT NULL,
    resource_plane_ids_json TEXT NOT NULL,
    required_capabilities_json TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
)
