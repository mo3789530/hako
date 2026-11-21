CREATE INDEX IF NOT EXISTS workspace_connection_leases_idle_idx
    ON workspace_connection_leases(workspace_id, resource_plane_id, allocation_generation, released_at, expires_at);
