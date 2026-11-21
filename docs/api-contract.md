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

`POST /{integration_id}/disable` and `/verify` require
`{"expected_revision": N}`. `/select-default` additionally requires
`expected_generation`; it only succeeds for a ready/active owned App with all
three approved purpose-specific credential readers and no legacy nonterminal
Workload Runs. It activates a ready integration and records the Tenant default
in one transaction. All nonterminal Runs pinned to another integration must be
drained first; a previous active Tenant-owned default is disabled with its
revision and credential generation advanced in the same transaction. Stale revision/generation and undrained Runs return HTTP
409 (`state_conflict` or `legacy_runs_not_drained`). Draft disable advances the configuration and
credential revisions. When the API has its trusted proof verifier configured,
`/verify` resolves the exact approved key pin, calls GitHub `GET /app` outside
the database transaction, then rechecks revision/generation before moving
`draft` to `ready`. Without that verifier it returns HTTP 409
`github_app_not_ready`; failed identity proof returns a sanitized 502
`github_app_verification_failed`. Verification alone does not activate or
select the App.

`POST /{integration_id}/migration` accepts an explicit `action` (`plan` or
`backfill`), `expected_revision`, `expected_generation`, and 1–100 unique
positive `installation_ids`. `plan` is read-only. `backfill` additionally
requires `confirm: true`, and inserts only rows whose stored, setup-verified
GitHub App ID matches the active integration. Unknown historical App identity,
inactive/suspended installations, and conflicting bindings are blocked; a
mixed batch rolls back atomically. Backfill never changes Workload Run pins or
the Tenant default. Stale pins and unsafe targets return 409. The backfill and
its safe audit summary commit in the same transaction. Other stable errors are
`invalid_request` (400), `idempotency_conflict` and `state_conflict` (409).
Create/disable audit metadata changes transactionally; idempotent create
replay does not append another event.

The matching CLI commands are `hako github app list`, `add`, `show`, `verify`,
`select-default`, `migration-plan`, `migration-backfill`, `deliveries`, and
`disable`.
Migration commands require `--revision`, `--generation`, explicit
`--installations`; applying a backfill also requires `--apply`. The CLI accepts only the opaque approved
binding ID and never accepts Secret values or arbitrary resource references.
Credential bindings are provisioned through an operator-only database path;
the API database role has read-only access to that table.

`GET /v1/tenants/{tenant_id}/github/apps/{integration_id}/deliveries` lists
failed deliveries for that Tenant integration. It uses `limit` (default 20,
range 1–100) and `offset` (range 0–1,000,000), and returns a `deliveries` page
with delivery ID, event/action, actor login, received/payload-purged timestamps,
and replay count. It never returns raw payload or normalized event content.
The matching CLI is `hako github app deliveries --tenant <tenant>
--integration <integration> [--limit N --offset N]`.

When both GitHub App management and webhook-triggered Workload Runs are enabled,
Tenant Owners/Admins can opt a synchronized repository in or out with
`PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/factory`
and `{"enabled":true|false}`. The route validates an explicit boolean and
returns repository metadata including `factory_enabled` and, when enabled,
`factory_enabled_at`. `hako github repository factory enable|disable --tenant
<tenant> --installation <id> --repository <id>` mirrors the API. The setting
defaults off. Disabling clears the opt-in timestamp; re-enabling records a new
timestamp, so deliveries received before that opt-in are not retroactively
scheduled. Even with the repository enabled, webhook trigger processing also
requires `HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED=true` and Workload Runs to be
enabled. The feature flag defaults off.

Tenant Owners/Admins can configure PR Preview metadata tracking independently
with `PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/preview`
and `{"enabled":true|false}`. The CLI is
`hako github repository preview enable|disable --tenant <tenant> --installation
<id> --repository <id>`. The default is off, opt-in is audited, and old
deliveries are not backfilled. This only records PR desired lifecycle state;
the API returns `runtime_enabled:false` until isolated Lambda MicroVM
allocation, expiring Gateway routes, and cleanup are available. See
[`Preview lifecycle`](preview-lifecycle.md).

Tenant members can list bounded Preview lifecycle metadata for an active
registered Repository through
`GET /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/previews?limit=50`
or `hako github repository previews list --tenant <tenant> --installation
<id> --repository <id> [--limit 50]`. The default limit is 50 and the maximum
is 100. Cross-Tenant Repository identifiers are hidden as not found. The
response continues to report `runtime_enabled:false`.

Tenant Owners/Admins can separately enable the per-repository GitHub Actions
runner admission policy with
`PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/actions-runner`
and `{"enabled":true|false}`. It returns `actions_runner_enabled` and, while
enabled, `actions_runner_enabled_at`. This policy is independent from the
Factory push/PR verification opt-in. `hako github repository runner
enable|disable --tenant <tenant> --installation <id> --repository <id>`
configures it. The default is disabled. Disabling clears the timestamp and a
subsequent enable records a new one. This setting alone does not request a
GitHub runner registration token, create a MicroVM, or execute a workflow job;
the dedicated, authorized ephemeral-runner consumer remains separate work.

An enabled repository can configure the optional Claude Action provenance
allowlist with
`PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/actions-runner/workflow-policy`.
The JSON body contains `workflow_path` (under `.github/workflows/`),
`allowed_refs` (branch refs only), `allowed_events` (`push`, `pull_request`,
`workflow_dispatch`, or `schedule`), `required_labels`, and `allowed_labels`.
Values are bounded and duplicate-free; required labels must be a subset of
allowed labels. `pull_request_target`, arbitrary commands, credentials,
image/role/network selectors, and unknown JSON fields are rejected. Only
Tenant Owners/Admins may configure it, changes are audited, and disabling the
repository runner opt-in clears this policy. Configure it with
`hako github repository runner policy --tenant <tenant> --installation <id>
--repository <id> --policy-file policy.json`.
Tenant members can read the active policy with
`GET /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/actions-runner/workflow-policy`
or `hako github repository runner policy show --tenant <tenant>
--installation <id> --repository <id>`. The endpoint returns the same
allowlist without exposing credentials; an absent policy or disabled runner
repository is returned as not found.

The broker launch path uses this allowlist with authoritative GitHub Run/Job
metadata before consuming a runner claim. Claim consumption rechecks that the
policy snapshot has not changed. A configured policy cannot be bypassed by the
legacy launch path. Tenant model-credential metadata is managed through
`/v1/tenants/{tenant_id}/model-credentials`; see
[`claude-model-credentials.md`](claude-model-credentials.md). Delivery to an
actual Action process remains in Task 75/76.

`POST /v1/tenants/{tenant_id}/github/apps/{integration_id}/deliveries/{delivery_id}/replay`
is available when Tenant GitHub App management is enabled and requires Tenant
Owner/Admin authorization. The body is `{"reason":"..."}` with an 8–500 byte
operator explanation. Only failed Tenant-owned deliveries with the current
active integration generation and Installation binding can be replayed. The
request preserves the original Delivery ID, is audited with actor and reason,
and is limited to three requests per delivery per hour. Success returns HTTP
202 with only `delivery_id` and `status`; stale/ineligible deliveries return
409, and rate-limited requests return 429 with `Retry-After`. Raw payloads and
normalized event contents are never returned by this endpoint.

Platform operators with a verified Cognito `hako-admin` group can inspect
failed Tenant-owned webhook deliveries across Tenants through
`GET /v1/admin/github/webhook-deliveries?limit=N&offset=N` (limit 1–100,
offset 0–1,000,000). The response contains only Tenant/integration/delivery,
event/action/actor, Installation/Repository IDs, received/payload-purged time,
replay count, and a replay-eligibility boolean. Legacy `shared` deliveries are
omitted because they do not have an unambiguous Tenant integration owner.
Tenant membership, including Owner/Admin, does not grant this platform route.

An operator may request an audited replay with
`POST /v1/admin/github/webhook-deliveries/{integration_id}/{delivery_id}/replay`
and the same 8–500 byte `reason` body. Only active Tenant-owned integrations,
current credential generations and active Installation bindings with an
unexpired, retained payload may be replayed. The request preserves the
original Delivery ID, uses the same three-per-hour limit as Tenant replay, and
stores actor and reason in `platform_audit_events` atomically with the state
change. It returns HTTP 202 with Tenant ID, Delivery ID and `received` status;
ineligible deliveries return 409 and rate-limited requests return 429. The
processor revalidates authorization before applying event side effects.

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

## Native Agent proposal publication

`GET /v1/tenants/{tenant_id}/workload-runs/{run_id}/agent-publication` returns
safe publication state only. When the API's Control Plane mTLS Broker client is
configured, `POST /v1/tenants/{tenant_id}/workload-runs/{run_id}/agent-publication/publish`
submits or retries the newest Agent attempt. The body must be empty. The caller
must be the Run requester or a Tenant Owner/Admin; other Tenant members and
cross-Tenant callers receive 404. The API chooses the latest attempt in its
database transaction, and the Broker independently revalidates the exact
attempt and all proposal/App/Repository/Check pins before a GitHub write.
Repeating the request is idempotent. Responses use `Cache-Control: no-store`
and contain only branch/PR metadata. Each authorized submit request is recorded
in the Tenant audit log with the selected attempt ID and no proposal or
credential data. The route is omitted unless the Broker
client is configured. The matching CLI is
`hako workload publication show|submit <tenant-id> <agent-run-id>`.
