CREATE TABLE IF NOT EXISTS tenant_github_default_integrations (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
    integration_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    FOREIGN KEY (tenant_id, integration_id) REFERENCES tenant_github_integrations(tenant_id, id)
)
