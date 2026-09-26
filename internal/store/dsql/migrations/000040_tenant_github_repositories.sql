CREATE TABLE IF NOT EXISTS tenant_github_repositories (
    tenant_id TEXT NOT NULL,
    installation_id BIGINT NOT NULL,
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    owner_login TEXT NOT NULL,
    repository_name TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT '',
    synchronized_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, installation_id, github_repository_id),
    FOREIGN KEY (tenant_id, installation_id) REFERENCES github_app_installation_bindings(tenant_id, installation_id)
)
