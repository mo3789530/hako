ALTER TABLE repository_preview_runtime_allocations
    ADD CONSTRAINT repository_preview_runtime_app_pin_tenant_fk
    FOREIGN KEY (tenant_id, github_integration_id)
    REFERENCES tenant_github_integrations(tenant_id, id)
