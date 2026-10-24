ALTER TABLE tenant_software_catalog_services
    ADD COLUMN IF NOT EXISTS oidc_deploy_policy JSONB,
    ADD CONSTRAINT tenant_software_catalog_services_oidc_deploy_policy_object
    CHECK (oidc_deploy_policy IS NULL OR jsonb_typeof(oidc_deploy_policy) = 'object')
