CREATE TABLE IF NOT EXISTS repository_preview_runtime_allocations (
    tenant_id TEXT NOT NULL,
    installation_id BIGINT NOT NULL,
    github_repository_id BIGINT NOT NULL,
    pull_request_number BIGINT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    head_sha TEXT NOT NULL CHECK (length(head_sha) = 40),
    head_ref TEXT NOT NULL CHECK (length(head_ref) BETWEEN 1 AND 1024),
    desired_state TEXT NOT NULL CHECK (desired_state IN ('present', 'absent')),
    state TEXT NOT NULL CHECK (state IN ('awaiting_runtime', 'running', 'cleanup_pending', 'cleaned')),
    expires_at TIMESTAMPTZ NOT NULL,
    resource_plane_id TEXT,
    workspace_id TEXT,
    microvm_id TEXT,
    gateway_route_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, installation_id, github_repository_id, pull_request_number, generation),
    FOREIGN KEY (tenant_id, installation_id, github_repository_id, pull_request_number)
        REFERENCES repository_previews(tenant_id, installation_id, github_repository_id, pull_request_number),
    CHECK ((resource_plane_id IS NULL AND workspace_id IS NULL AND microvm_id IS NULL AND gateway_route_id IS NULL)
        OR (resource_plane_id IS NOT NULL AND workspace_id IS NOT NULL AND microvm_id IS NOT NULL AND gateway_route_id IS NOT NULL)),
    CHECK (state <> 'running' OR (resource_plane_id IS NOT NULL AND workspace_id IS NOT NULL AND microvm_id IS NOT NULL AND gateway_route_id IS NOT NULL))
)
