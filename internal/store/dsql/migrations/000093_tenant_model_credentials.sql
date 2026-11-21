CREATE TABLE IF NOT EXISTS tenant_model_credentials (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider = 'anthropic'),
    binding_id TEXT NOT NULL,
    secret_version_id TEXT NOT NULL CHECK (secret_version_id ~ '^[A-Fa-f0-9]{32}$'),
    status TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    credential_generation BIGINT NOT NULL CHECK (credential_generation > 0),
    created_by TEXT NOT NULL REFERENCES users(id),
    updated_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id, binding_id) REFERENCES tenant_model_credential_bindings(tenant_id, id),
    UNIQUE (tenant_id, provider)
);
