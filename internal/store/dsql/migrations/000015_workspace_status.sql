CREATE TABLE IF NOT EXISTS workspace_status (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
    desired_state TEXT NOT NULL CHECK (desired_state IN ('running', 'suspended', 'deleted')),
    observed_state TEXT NOT NULL CHECK (observed_state IN ('pending', 'provisioning', 'running', 'suspending', 'suspended', 'deleting', 'deleted', 'failed')),
    updated_at TIMESTAMPTZ NOT NULL
);
