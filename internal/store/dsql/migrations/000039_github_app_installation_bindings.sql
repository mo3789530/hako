CREATE TABLE IF NOT EXISTS github_app_installation_bindings (
    installation_id BIGINT PRIMARY KEY CHECK (installation_id > 0),
    tenant_id TEXT NOT NULL,
    bound_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, installation_id) REFERENCES tenant_github_installations(tenant_id, installation_id),
    UNIQUE (tenant_id, installation_id)
)
