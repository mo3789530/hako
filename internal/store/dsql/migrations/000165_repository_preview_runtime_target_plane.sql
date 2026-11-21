ALTER TABLE repository_preview_runtime_allocations
    ADD COLUMN IF NOT EXISTS target_resource_plane_id TEXT;
