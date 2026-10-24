ALTER TABLE repository_preview_runtime_allocations
    ADD CONSTRAINT repository_preview_runtime_target_plane_binding_check
    CHECK (resource_plane_id IS NULL OR target_resource_plane_id IS NULL OR resource_plane_id = target_resource_plane_id);
