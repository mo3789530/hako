CREATE INDEX IF NOT EXISTS repository_preview_runtime_cleanup_due_idx
    ON repository_preview_runtime_allocations (COALESCE(runtime_cleanup_next_attempt_at, updated_at), tenant_id, installation_id, github_repository_id, pull_request_number, generation)
    WHERE desired_state = 'absent' AND state = 'cleanup_pending';
