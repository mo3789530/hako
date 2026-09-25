CREATE TABLE IF NOT EXISTS operations (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    resource_plane_id TEXT NOT NULL REFERENCES resource_planes(id),
    type TEXT NOT NULL CHECK (type IN ('ensure_running', 'suspend', 'resume', 'delete')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, idempotency_key)
);
