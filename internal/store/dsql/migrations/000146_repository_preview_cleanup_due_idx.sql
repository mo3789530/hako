CREATE INDEX IF NOT EXISTS repository_previews_cleanup_due_idx
    ON repository_previews(cleanup_next_attempt_at, cleanup_lease_expires_at, updated_at,
        tenant_id, installation_id, github_repository_id, pull_request_number)
    WHERE desired_state = 'absent' AND state = 'cleanup_pending';
