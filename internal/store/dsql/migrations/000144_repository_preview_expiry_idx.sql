CREATE INDEX IF NOT EXISTS repository_previews_expiry_idx
    ON repository_previews(expires_at, tenant_id, installation_id, github_repository_id, pull_request_number)
    WHERE desired_state = 'present' AND expires_at IS NOT NULL;
