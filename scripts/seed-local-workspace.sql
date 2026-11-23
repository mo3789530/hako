BEGIN;

INSERT INTO tenants (id, name, created_at)
VALUES ('ten_local', 'Local Cloud Hypervisor Development', now())
ON CONFLICT (id) DO NOTHING;

WITH local_user AS (
    INSERT INTO users (id, cognito_subject, email, created_at)
    VALUES ('usr_local', 'hako-local-user', '', now())
    ON CONFLICT (cognito_subject) DO UPDATE SET email = EXCLUDED.email
    RETURNING id
)
INSERT INTO tenant_members (tenant_id, user_id, role, joined_at)
SELECT 'ten_local', id, 'owner', now() FROM local_user
ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = 'owner';

INSERT INTO resource_planes (id, provider, region, capabilities_json)
VALUES ('rp-local', 'cloud-hypervisor', 'local', '["microvm"]')
ON CONFLICT (id) DO UPDATE SET provider = EXCLUDED.provider, region = EXCLUDED.region,
    capabilities_json = EXCLUDED.capabilities_json;

INSERT INTO resource_plane_status (resource_plane_id, status, updated_at)
VALUES ('rp-local', 'active', now())
ON CONFLICT (resource_plane_id) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at;

INSERT INTO resource_plane_capacities (resource_plane_id, max_workspaces, reserved_workspaces, updated_at)
VALUES ('rp-local', 10, 0, now())
ON CONFLICT (resource_plane_id) DO UPDATE SET max_workspaces = EXCLUDED.max_workspaces,
    reserved_workspaces = EXCLUDED.reserved_workspaces, updated_at = EXCLUDED.updated_at;

COMMIT;
