ALTER TABLE tenant_model_credentials
    ADD CONSTRAINT tenant_model_credentials_purpose_check
    CHECK (purpose IN ('claude_action', 'native_agent'));
