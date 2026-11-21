CREATE TABLE IF NOT EXISTS workspaces (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    owner_id TEXT NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    runtime_class TEXT NOT NULL,
    image TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);
