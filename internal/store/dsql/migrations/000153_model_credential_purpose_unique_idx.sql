CREATE UNIQUE INDEX IF NOT EXISTS tenant_model_credentials_tenant_provider_purpose_idx
    ON tenant_model_credentials (tenant_id, provider, purpose);
