# Tenant Software Catalog

The Software Catalog stores Tenant-owned service metadata for the Hako
Software Factory. It is separate from the built-in Workspace Service Catalog
(`postgres`, `redis`) and does not create or deploy a runtime.

## API and CLI

Tenant members can list catalog entries. Tenant Owners and Admins can create
or update an entry:

```text
GET /v1/tenants/{tenant_id}/software-catalog
PUT /v1/tenants/{tenant_id}/software-catalog/{service_id}
```

```bash
hako catalog list --tenant acme
hako catalog set --tenant acme --service payments-api --file payments-api.json
```

Example metadata file:

```json
{
  "owner_team": "payments",
  "installation_id": 12345,
  "github_repository_id": 987654321,
  "runtime": "lambda-microvm",
  "language": "go",
  "dependencies": ["identity-api"],
  "production_url": "https://payments.example.com"
}
```

`service_id` is supplied in the URL/CLI and is a stable lowercase identifier.
The Tenant and Repository must already exist in Hako's registry; Repository
ownership is enforced by a composite foreign key and is rechecked on every
write. Each dependency must already be registered in the same Tenant. Create
dependencies before services that refer to them. Supported runtime metadata
is currently `lambda-microvm`; EC2 is not supported. Language values are
`go`, `java`, `node`, `python`, `rust`, `dotnet`, and `other`.

Production URLs must use HTTPS and cannot contain user information or a
fragment. Catalog rows contain no credential fields. GitHub Repository names
are derived from Hako's registry, and write audit events record identity,
runtime/language, dependency count, and whether a URL is configured, not URL
contents or secrets. Owners/Admins may update an existing entry; delete and
dependency-cycle management are not implemented yet.

## OIDC deployment policy (dormant)

Owners/Admins can configure a strict allowlist for a future trusted deployment
workload; Tenant members can view it:

```text
GET /v1/tenants/{tenant_id}/software-catalog/{service_id}/deploy-policy
PUT /v1/tenants/{tenant_id}/software-catalog/{service_id}/deploy-policy
POST /v1/tenants/{tenant_id}/software-catalog/{service_id}/deploy-policy/verify
```

```bash
hako catalog deploy-policy show --tenant acme --service payments-api
hako catalog deploy-policy set --tenant acme --service payments-api --file deploy-policy.json
hako catalog deploy-policy verify --tenant acme --service payments-api --token-file github-oidc.jwt
```

An enabled policy requires the exact `sts.amazonaws.com` audience, sorted
exact branch/tag refs, named environments, exact workflow refs, and the
explicit `require_human_approval` setting. Wildcards and pull-request refs are
rejected. Verification validates the RS256 signature using keys from the fixed
GitHub Actions issuer, then checks issuer, single exact audience, registered
Repository name and numeric ID, Owner ID, `sub`, ref, environment, event,
workflow and reusable-workflow claims. Both legacy name-based `sub` and the
immutable Owner/Repository-ID format are accepted, while the signed numeric
IDs must match the Tenant Catalog Repository. OIDC tokens are accepted in memory for this
verification request only; they are not persisted or returned.

This is a claim-verification endpoint only. It does not exchange the token at
AWS STS, issue cloud credentials, or authorize a deployment; API responses
always set `execution_enabled` to `false`. `require_human_approval` records
policy intent but does not inspect GitHub Environment protection settings or
prove that a human approved a deployment. Those controls and deployment
execution remain disabled pending a trusted approval integration, IAM trust
configuration, Preview lifecycle/routing and production deployment review.
See [Task 54](../tasks/task-54-preview-catalog-deploy.md).
For PR Preview opt-in and the currently metadata-only lifecycle, see
[Preview lifecycle](preview-lifecycle.md).
