CREATE INDEX IF NOT EXISTS tenant_model_credentials_active_idx
    ON tenant_model_credentials (tenant_id, provider, credential_generation)
    WHERE status = 'active';
