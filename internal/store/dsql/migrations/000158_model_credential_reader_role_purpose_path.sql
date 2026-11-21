ALTER TABLE tenant_model_credential_bindings
    DROP CONSTRAINT IF EXISTS tenant_model_credential_bindings_reader_role_arn_check,
    ADD CONSTRAINT tenant_model_credential_bindings_reader_role_arn_purpose_path_check
        CHECK (reader_role_arn ~ '^arn:aws:iam::[0-9]{12}:role/hako/tenant-model-credential-reader/[A-Za-z0-9_-]{1,128}$');
