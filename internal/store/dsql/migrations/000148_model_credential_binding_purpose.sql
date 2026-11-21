ALTER TABLE tenant_model_credential_bindings
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'claude_action';
