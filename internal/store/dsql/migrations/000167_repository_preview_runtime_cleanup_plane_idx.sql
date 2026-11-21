CREATE INDEX IF NOT EXISTS repository_preview_runtime_cleanup_plane_idx
    ON repository_preview_runtime_allocations
       (target_resource_plane_id, runtime_cleanup_next_attempt_at, updated_at)
    WHERE desired_state = 'absent'
      AND state = 'cleanup_pending'
      AND target_resource_plane_id IS NOT NULL;
