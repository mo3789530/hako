CREATE TABLE IF NOT EXISTS repository_previews (
    tenant_id TEXT NOT NULL,
    installation_id BIGINT NOT NULL,
    github_repository_id BIGINT NOT NULL,
    pull_request_number BIGINT NOT NULL CHECK (pull_request_number > 0),
    head_sha TEXT NOT NULL CHECK (length(head_sha) = 40),
    head_ref TEXT NOT NULL,
    desired_state TEXT NOT NULL CHECK (desired_state IN ('present', 'absent')),
    state TEXT NOT NULL CHECK (state IN ('awaiting_runtime', 'cleanup_pending', 'cleaned')),
    last_delivery_id TEXT NOT NULL,
    last_event_at TIMESTAMPTZ NOT NULL,
    cleanup_attempts INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_attempts >= 0),
    cleanup_next_attempt_at TIMESTAMPTZ,
    cleanup_last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, installation_id, github_repository_id, pull_request_number),
    FOREIGN KEY (tenant_id, installation_id, github_repository_id)
        REFERENCES tenant_github_repositories(tenant_id, installation_id, github_repository_id)
)
