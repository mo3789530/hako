CREATE TABLE IF NOT EXISTS github_app_registrations (
    id TEXT PRIMARY KEY,
    github_app_id BIGINT NOT NULL UNIQUE CHECK (github_app_id > 0),
    mode TEXT NOT NULL CHECK (mode IN ('shared', 'tenant_owned')),
    owner_tenant_id TEXT REFERENCES tenants(id),
    slug TEXT NOT NULL,
    client_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((mode = 'shared' AND owner_tenant_id IS NULL) OR (mode = 'tenant_owned' AND owner_tenant_id IS NOT NULL)),
    UNIQUE (id, github_app_id)
)
