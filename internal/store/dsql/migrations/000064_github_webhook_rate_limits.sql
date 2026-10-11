CREATE TABLE IF NOT EXISTS github_webhook_rate_limits (
    integration_id TEXT PRIMARY KEY REFERENCES tenant_github_integrations(id),
    window_started_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL CHECK (attempts > 0)
)
