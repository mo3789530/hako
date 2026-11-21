CREATE TABLE IF NOT EXISTS tenant_members (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    user_id TEXT NOT NULL REFERENCES users(id),
    role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    joined_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);
