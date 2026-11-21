CREATE TABLE IF NOT EXISTS workspace_quota_slots (
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    slot SMALLINT NOT NULL CHECK (slot BETWEEN 1 AND 10),
    workspace_id TEXT NOT NULL UNIQUE REFERENCES workspaces(id),
    PRIMARY KEY (tenant_id, slot)
);
