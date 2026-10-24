UPDATE repository_preview_runtime_allocations
SET target_resource_plane_id = resource_plane_id
WHERE target_resource_plane_id IS NULL
  AND resource_plane_id IS NOT NULL;
