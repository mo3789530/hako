ALTER TABLE tenant_model_credentials
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'claude_action';
