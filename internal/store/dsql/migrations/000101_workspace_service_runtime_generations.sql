CREATE TABLE IF NOT EXISTS workspace_service_runtime_generations (
    service_id TEXT NOT NULL REFERENCES services(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    allocation_generation BIGINT NOT NULL CHECK (allocation_generation > 0),
    microvm_id TEXT NOT NULL,
    desired_revision BIGINT NOT NULL CHECK (desired_revision > 0),
    observed_state TEXT NOT NULL CHECK (observed_state IN ('healthy', 'unhealthy', 'stopped', 'deleted', 'failed')),
    retired BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (service_id, resource_plane_id, allocation_generation)
);
