ALTER TABLE tenant_model_credential_bindings
    ADD CONSTRAINT tenant_model_credential_bindings_purpose_check
    CHECK (purpose IN ('claude_action', 'native_agent'));
