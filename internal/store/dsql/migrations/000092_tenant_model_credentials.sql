-- Operator-provisioned bindings point to one Tenant-owned, purpose-specific
-- Secrets Manager Secret. The public API can reference approved bindings but
-- cannot create or rewrite them.
CREATE TABLE IF NOT EXISTS tenant_model_credential_bindings (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider = 'anthropic'),
    secret_arn TEXT NOT NULL CHECK (secret_arn ~ '^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$'),
    reader_role_arn TEXT NOT NULL CHECK (reader_role_arn ~ '^arn:[^:]+:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]+$'),
    status TEXT NOT NULL CHECK (status IN ('approved', 'revoked')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, id),
    UNIQUE (secret_arn)
);

