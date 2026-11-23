CREATE TABLE workspace_github_checkouts (
    tenant_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL PRIMARY KEY,
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    owner_login TEXT NOT NULL,
    repository_name TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    commit_sha TEXT,
    checkout_state TEXT NOT NULL CHECK (checkout_state IN ('pending', 'ready', 'failed')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, workspace_id) REFERENCES workspaces(tenant_id, id),
    CHECK (commit_sha IS NULL OR commit_sha ~ '^[a-fA-F0-9]{40}$' OR commit_sha ~ '^[a-fA-F0-9]{64}$')
);
