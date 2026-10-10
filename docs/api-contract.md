# Control Plane API Contract

The public Control Plane API is versioned under `/v1`. This document records the
implemented request, error, pagination, and audit conventions so the CLI and API
can evolve independently.

## Health endpoints

`GET /healthz` is an unauthenticated, minimal liveness endpoint for load
balancers and Lambda Web Adapter readiness checks. It returns only
`{"status":"ok"}` and does not disclose database, Tenant, or Workspace
details. `GET /v1/health` remains part of the protected API and requires a
valid Cognito access token with the `hako/api` scope. API Gateway explicitly
exempts only `GET /healthz`; its `$default` route protects all other paths.

## Request validation

JSON mutation endpoints accept one JSON object, reject unknown fields, reject
trailing JSON values, and cap the body at 16 KiB. Oversized input returns HTTP
413 with `request_too_large`; malformed or semantically invalid input returns
HTTP 400 with `invalid_request` (the workspace list has the more specific
`invalid_pagination` code). Resource Plane placement policy validation also
enforces at most 50 unique, non-empty selectors per list.

## Error envelope

All API errors use the same JSON envelope:

```json
{"error":{"code":"invalid_request","message":"..."}}
```

Clients should branch on the stable `code`, not the human-readable `message`.
Tenant- and Workspace-level authorization failures intentionally use the same
404 response as missing resources to avoid disclosing their existence.

## Workspace list pagination

`GET /v1/tenants/{tenant_id}/workspaces` accepts `limit` (default 20, range
1–100) and `offset` (default 0, range 0–1,000,000). Unknown parameters,
duplicate parameters, empty values, and out-of-range/non-integer values return
HTTP 400 `invalid_pagination`. The response contains `items`, `total`, `limit`,
and `offset`; cursor pagination is not implemented yet.

## Tenant GitHub App metadata

Tenant App metadata routes are registered only when
`HAKO_GITHUB_APP_MANAGEMENT_ENABLED=true`. They require a Cognito access token
with `hako/api`, a Hako user, and Owner/Admin membership in the path Tenant.
Other members and callers from another Tenant receive the hidden 404 response.

`GET /v1/tenants/{tenant_id}/github/apps` accepts `limit` (default 50, range
1–100) and `offset` (range 0–10,000). `POST` on that path creates an inert
draft and requires `Idempotency-Key` plus JSON fields `github_app_id`, `slug`,
`client_id`, and `credential_binding_id`. The binding must already be approved
for that exact Tenant and App ID. The API rejects unknown fields, including
Secret, ARN, Role, and URL inputs. `GET /{integration_id}` returns the same
sanitized metadata shape. Responses omit the binding ID and all credential
references.

`POST /{integration_id}/disable`, `/verify`, and `/select-default` require
`{"expected_revision": N}`. Draft disable advances the configuration and
credential revisions. When the API has its trusted proof verifier configured,
`/verify` resolves the exact approved key pin, calls GitHub `GET /app` outside
the database transaction, then rechecks revision/generation before moving
`draft` to `ready`. Without that verifier it returns HTTP 409
`github_app_not_ready`; failed identity proof returns a sanitized 502
`github_app_verification_failed`. `/select-default` remains unimplemented and
returns 409; verification alone does not activate or select the App. Other stable errors are `invalid_request` (400),
`idempotency_conflict` and `state_conflict` (409). Create and disable append a
safe audit summary in the same transaction as metadata changes; idempotent
create replay does not append another event.

The matching CLI commands are `hako github app list`, `add`, `show`, `verify`,
`select-default`, and `disable`. The CLI accepts only the opaque approved
binding ID and never accepts Secret values or arbitrary resource references.
Credential bindings are provisioned through an operator-only database path;
the API database role has read-only access to that table.

With an explicitly injected trusted `TenantGitHubSetupResolver`, metadata
management also exposes `POST /{integration_id}/setup` with `{}`. Only active
owned integrations with approved bindings are eligible. Each request creates
fresh one-time state pinned to Tenant, integration, App registration/App ID,
revision and generation. Both redirect and OAuth callback resolve their
verifier from that stored pin; callback query Tenant/App IDs are not used.
Wrong-App verification returns 401, expired/changed/consumed state returns
400, and an unavailable broker returns sanitized 502
`github_setup_unavailable`. Final binding creation rechecks Owner/Admin and
the pin transactionally. The production API can be configured with
`HAKO_TENANT_GITHUB_OAUTH_BROKER_URL`, `HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE`,
`HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE`, and
`HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE`; all four are required together and
the App management feature flag must also be enabled. The API sends only its
mTLS client identity and database-resolved pin to the broker. The CLI command
`hako github app setup --tenant <tenant> --integration <integration>` starts
the one-time flow and opens the returned GitHub URL. Broker endpoint deployment
and certificate provisioning remain separate operations.

## Audit events

Workspace create, suspend, resume, and delete requests, plus Tenant placement
policy updates, append an immutable actor-oriented row to `audit_events` in the
same database transaction as the desired-state/policy mutation and outbox
command. Platform administrator updates to Resource Plane Health append a row
to `platform_audit_events` in the same transaction as the health report. The
platform audit stream has no Tenant owner because these operations target
platform resources. Its current writer stores the actor, action, target,
timestamp, and safe status summary; it omits free-form Health reason text. There
is not yet a public audit read/export API. Health responses identify whether
the latest report came from an `operator` or `automated` source. Automated
reports are recorded in `resource_plane_health_events`, without creating a
synthetic human actor; their authenticated ingress and audit export remain
future work. Idempotent replays do not create
duplicate audit rows. Tenant audit rows record the Tenant, actor User, action,
target, safe summary details, and UTC occurrence time. Workspace lifecycle Operation history remains separate: it records
asynchronous execution and system/reconciler transitions, while `audit_events`
records who requested an API mutation.

The audit table is append-only by application convention. The reviewed DSQL
bootstrap template now revokes API UPDATE/DELETE on actor audit tables, but
this grant change has not been applied to an AWS deployment. Read/export API,
retention policy, and audit of read-only API access remain unconfigured. Do not put credentials, tokens, or secret
values into event details.

## Related design decisions

The compatibility rules for `/v1` and versioned asynchronous messages are in
[ADR 0003](adr/0003-api-compatibility.md). API Gateway's JWT scope and Lambda
integration boundary is described in [API Gateway and Lambda](api-gateway-lambda.md).
