CREATE TABLE IF NOT EXISTS repository_preview_sessions (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{48}$'),
    user_id TEXT NOT NULL REFERENCES users(id),
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    pull_request_number BIGINT NOT NULL CHECK (pull_request_number > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    workspace_id TEXT NOT NULL,
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    permissions_json TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, installation_id, github_repository_id, pull_request_number, generation)
        REFERENCES repository_preview_runtime_allocations(tenant_id, installation_id, github_repository_id, pull_request_number, generation)
);
