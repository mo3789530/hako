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

The audit table is append-only by application convention, but database-level
role restrictions, read/export API, retention policy, and audit of read-only
API access are not yet configured. Do not put credentials, tokens, or secret
values into event details.

## Related design decisions

The compatibility rules for `/v1` and versioned asynchronous messages are in
[ADR 0003](adr/0003-api-compatibility.md). API Gateway's JWT scope and Lambda
integration boundary is described in [API Gateway and Lambda](api-gateway-lambda.md).
