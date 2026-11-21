CREATE TABLE IF NOT EXISTS tenant_github_installations (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    account_login TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'revoked')),
    requested_by TEXT NOT NULL REFERENCES users(id),
    requested_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, installation_id)
)
