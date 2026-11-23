CREATE TABLE IF NOT EXISTS tenant_repository_members (
    tenant_id TEXT NOT NULL,
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    user_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('viewer', 'operator')),
    membership_joined_at TIMESTAMPTZ NOT NULL,
    installation_bound_at TIMESTAMPTZ NOT NULL,
    granted_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, installation_id, github_repository_id, user_id)
)
