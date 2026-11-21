CREATE INDEX IF NOT EXISTS workspace_connection_leases_active_idx
    ON workspace_connection_leases(workspace_id, resource_plane_id, allocation_generation, expires_at)
    WHERE released_at IS NULL;
