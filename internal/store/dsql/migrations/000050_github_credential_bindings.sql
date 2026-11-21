CREATE TABLE IF NOT EXISTS github_credential_bindings (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    github_app_id BIGINT NOT NULL CHECK (github_app_id > 0),
    status TEXT NOT NULL CHECK (status IN ('approved', 'disabled')),
    private_key_secret_ref TEXT NOT NULL,
    oauth_secret_ref TEXT NOT NULL,
    webhook_secret_ref TEXT NOT NULL,
    approved_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, id, github_app_id)
)
