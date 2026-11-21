CREATE TABLE IF NOT EXISTS tenant_software_catalog_services (
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    service_id TEXT NOT NULL CHECK (service_id ~ '^[a-z][a-z0-9-]{0,62}$'),
    owner_team TEXT NOT NULL,
    installation_id BIGINT NOT NULL,
    github_repository_id BIGINT NOT NULL CHECK (github_repository_id > 0),
    runtime TEXT NOT NULL CHECK (runtime = 'lambda-microvm'),
    language TEXT NOT NULL,
    dependencies JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(dependencies) = 'array'),
    production_url TEXT,
    created_by TEXT NOT NULL REFERENCES users(id),
    updated_by TEXT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, service_id),
    FOREIGN KEY (tenant_id, installation_id, github_repository_id)
        REFERENCES tenant_github_repositories(tenant_id, installation_id, github_repository_id)
)
