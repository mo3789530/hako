CREATE INDEX IF NOT EXISTS workspace_service_runtime_generations_active_idx
    ON workspace_service_runtime_generations(workspace_id, resource_plane_id, allocation_generation)
    WHERE retired = FALSE;
