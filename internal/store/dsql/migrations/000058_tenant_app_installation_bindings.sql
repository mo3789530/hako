CREATE TABLE IF NOT EXISTS tenant_github_app_installation_bindings (
    tenant_id TEXT NOT NULL,
    integration_id TEXT NOT NULL,
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    bound_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, integration_id, installation_id),
    UNIQUE (installation_id),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_github_integrations(tenant_id, id),
    FOREIGN KEY (tenant_id, installation_id) REFERENCES tenant_github_installations(tenant_id, installation_id)
)
