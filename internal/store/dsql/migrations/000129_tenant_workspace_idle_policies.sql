CREATE TABLE tenant_workspace_idle_policies (
    tenant_id VARCHAR(128) PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    grace_period_seconds INTEGER NOT NULL DEFAULT 900,
    revision BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT tenant_workspace_idle_policies_grace_check CHECK (grace_period_seconds BETWEEN 300 AND 86400)
);
