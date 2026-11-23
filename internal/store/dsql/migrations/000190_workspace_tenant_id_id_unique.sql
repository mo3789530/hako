ALTER TABLE workspaces ADD CONSTRAINT workspaces_tenant_id_id_unique UNIQUE (tenant_id, id);
