# GitHub Webhook Processor

The webhook API authenticates and durably records a delivery; it does not run
event side effects on the request path. `cmd/hako-webhook-processor-lambda`
drains supported Installation lifecycle, repository-membership, and (when
explicitly enabled) repository trigger events from that inbox.

## Processing contract

- A batch is bounded to 10 deliveries by the scheduled Lambda (the store
  supports an explicit maximum of 100).
- Schema-v1 `installation_repositories` events are selected only for active,
  non-suspended Installation-to-Tenant bindings. Installation `created`,
  `deleted`, `suspend`, and `unsuspend` events are processed by installation
  ID. With `HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED=true`, push and pull-request
  deliveries are additionally consumed; `workflow_job` and older schema
  deliveries stay in `received` for their appropriate future consumer.
- Push and pull-request scheduling is opt-in per synchronized Tenant
  repository. A Tenant Owner/Admin uses
  `PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/factory`
  with `{"enabled":true}` (or `hako github repository factory enable`). The
  default is disabled. The API rejects enabling unless Workload Run creation
  is enabled. `factory_enabled_at` prevents a delivery received before opt-in
  from being scheduled if it was already waiting in the inbox; disabling
  clears the timestamp, and re-enabling starts a new opt-in window.
- The initial automatic trigger is fixed and untrusted: push, and pull-request
  `opened`, `reopened`, or `synchronize` create a `standard` `job` Run with a
  30-minute timeout. Other supported pull-request actions are consumed without
  a Run; branch-deletion pushes are also consumed without a Run. Commit/ref
  come from the authenticated normalized event. This is not
  GitHub Actions `workflow_job` execution; that is Task 52 and must verify
  workflow/job provenance before provisioning.
- Each delivery's repository sync and `received` → `processed` update share a
  transaction. A retry therefore cannot partially sync a repository or apply
  the same event twice. Both lifecycle and repository-membership handlers
  re-read the candidate delivery with `SELECT ... FOR UPDATE` before side
  effects; PostgreSQL serializes the same delivery and Aurora DSQL adjudicates
  this read with OCC. The shared transaction runner retries OCC conflicts, so
  the loser re-reads the committed state and becomes a no-op.
- Installation events never establish Tenant ownership. `created` is an
  authorization-neutral no-op; `suspend` blocks active Repository access,
  `unsuspend` restores access only for an already-active claim, and `deleted`
  revokes the claim and deletes its synchronized Repository rows. An unbound
  Installation lifecycle event is acknowledged as processed without creating
  a claim or binding. The global Installation binding is retained after
  revocation to prevent automatic reassignment to a different Tenant; operator
  reassignment tooling is not implemented.
- Tenant-owned deliveries are keyed by `(integration_id, delivery_id)` and
  carry the credential generation that verified their signature. Before
  applying lifecycle or repository-membership changes, the processor
  rechecks that the integration is still active at that generation and that
  the Installation is bound to that integration. Stale or revoked deliveries
  are marked `failed` (quarantined) rather than mutating Tenant state. The
  legacy shared route uses the reserved `shared` integration and generation
  zero.
- Original signed request bodies expire seven days after receipt. Each bounded
  processor batch purges up to 1,000 expired bodies by clearing `payload_json`
  and setting `payload_purged_at`; the normalized event, SHA-256 digest, actor,
  and delivery metadata remain. Duplicate delivery detection therefore still
  works after body purge. The database migration backfills this deadline for
  existing inbox rows.
- A Tenant Owner/Admin may request replay of a failed Tenant-owned delivery
  with `POST /v1/tenants/{tenant_id}/github/apps/{integration_id}/deliveries/{delivery_id}/replay`
  and a JSON `reason` (8–500 bytes). The API returns only the delivery ID and
  `received` status, never the payload. Replay rechecks the active App,
  credential generation, and Installation binding, records actor/reason in the
  audit log in the same transaction, preserves the original Delivery ID, and
  permits at most three requests per delivery per hour. The processor repeats
  authorization before applying side effects. Stale or non-failed deliveries
  cannot be replayed.
- Tenant Owners/Admins can inspect failed deliveries through the matching
  `GET /v1/tenants/{tenant_id}/github/apps/{integration_id}/deliveries` route
  or `hako github app deliveries`. The paginated response contains only
  delivery/event metadata and replay count; it excludes raw and normalized
  payload contents. The query uses the failed-delivery partial index added by
  migration `000073`.
- A verified platform `hako-admin` can inspect failed Tenant-owned deliveries
  across Tenants through `GET /v1/admin/github/webhook-deliveries` and request
  replay through `POST /v1/admin/github/webhook-deliveries/{integration_id}/{delivery_id}/replay`.
  Results are paginated/redacted and omit legacy `shared` rows, which have no
  unambiguous Tenant owner. Tenant Owner/Admin membership alone is insufficient.
  Replay requires an active owned integration and current credential generation,
  an active Installation binding, and an unexpired retained raw payload. The
  update and actor/reason `platform_audit_events` entry are atomic and obey the
  same three-replays-per-hour limit; the processor repeats authorization.
- When present, the normalized Installation App ID must match the registered
  App at both ingress and processing. Database lookup errors propagate for
  retry; they do not quarantine an otherwise valid event.
- A database error fails the Lambda invocation so EventBridge retries. The
  durable inbox remains the source of truth. Unbound repository-membership
  events are not selected; unbound lifecycle events are processed as no-ops,
  so neither causes a hot retry loop.
- For trigger events, the active Tenant binding, repository opt-in, and
  delivery receipt time are rechecked in the same transaction that creates the
  Delivery-ID-idempotent Workload Run, queued Check, and Outbox event and marks
  the delivery processed. Duplicate processing of an already processed
  delivery is a no-op.

## Local verification

The integration suite uses isolated PostgreSQL schemas and exercises
active-binding selection, lifecycle transitions, idempotency, and pending
events:

```sh
HAKO_TEST_DATABASE_URL='postgres://…' go test -tags=integration ./internal/store/githubwebhook ./internal/store/githubregistry ./internal/workloadjobs -count=1
make build-webhook-processor-lambda
```

The integration test helper creates isolated temporary schemas in the
configured local PostgreSQL database and leaves the database server running.
The scheduled Lambda reads
`HAKO_DSQL_HOST`, `HAKO_DSQL_USER`, and `HAKO_DSQL_DATABASE`; locally it also
accepts `HAKO_DATABASE_URL`.

## AWS enablement

The Terraform integration is opt-in and defaults off. Before enabling it:

1. Apply the Control Plane Terraform to create the stable processor IAM role.
2. Run DSQL migrations, including `000044` (Installation suspension state) and
   `000045` (lifecycle pending-event index).
3. Replace `REPLACE_WITH_GITHUB_WEBHOOK_PROCESSOR_ROLE_ARN` in
   `infra/terraform/modules/control-plane/bootstrap-api-role.sql.tmpl` with
   output `github_webhook_processor_role_arn`; create/map the custom DSQL role
   and apply its table-level grants as the migration admin. If triggers are
   enabled, review the additional narrowly scoped Run/Check/Outbox grants.
4. Build the ZIP with `make build-webhook-processor-lambda`. Keep
   `enable_webhook_workload_triggers=false` by default. Enabling it requires
   `enable_github_webhook_processor`, `enable_workload_run_creation`,
   `enable_github_checks_delivery`, and `enable_outbox_dispatcher`; any apply
   requires a separately reviewed and approved plan.
5. Confirm successful scheduled invocations and monitor the processor Errors
   alarm before relying on automatic delivery processing.

The runtime IAM role has only CloudWatch log-write and `dsql:DbConnect` access
to the Control Plane cluster. It has no Secrets Manager, GitHub API, SQS, or
schema-admin permission. SQL table grants are limited to the inbox, active
Tenant binding/repository registry, and the exact Run/Check/Outbox writes
required when trigger mode is enabled.
Never enable the schedule before applying the migration and custom DSQL role
mapping. No Terraform apply is performed by local development or CI.

## Platform operator dead-letter procedure

1. Sign in with an operator account whose verified Cognito token has the
   `hako-admin` group and `hako/api` scope. A Tenant Owner/Admin account is not
   sufficient for the platform-wide endpoints.
2. List failed Tenant-owned deliveries with
   `GET /v1/admin/github/webhook-deliveries?limit=20&offset=0`. Follow the
   returned pagination fields. Treat `replay_eligible=false` as inspect-only;
   the endpoint intentionally omits raw payloads and legacy `shared` rows.
3. Before replay, verify the Tenant, integration, Installation, event and
   current operational incident from the redacted metadata and correlated
   processor logs. Do not copy credentials or webhook bodies into the reason.
4. For an approved transient failure, submit
   `POST /v1/admin/github/webhook-deliveries/{integration_id}/{delivery_id}/replay`
   with an 8–500 byte reason. Preserve the returned Delivery ID for tracking.
   A replay is rejected if the integration/generation/Installation is no longer
   active, payload retention expired, the delivery is not failed, or the
   per-delivery limit (three per hour) is reached.
5. Confirm the existing processor observes the requeued delivery and inspect
   its terminal status through the redacted list. Do not manually edit inbox
   rows to bypass eligibility. Escalate persistent failures to the service
   owner; each accepted replay is recorded in `platform_audit_events`.

These steps describe the local API contract; production operator account/group
provisioning, monitoring, and deployment remain separate operational gates.

## Remaining work

This processor does not verify GitHub's setup callback or activate pending
Installation claims. It does not process `workflow_job`, call GitHub APIs, or
run a GitHub Actions runner. The platform operator API is implemented locally;
the broader retention/replay/trigger E2E and a detailed operator replay runbook
remain tracked by Tasks 78–79. The opt-in Terraform is not applied, and the
downstream runtime executor is not deployed; keep trigger mode disabled until
the Check consumer and MicroVM-backed Workload worker are deployed and monitored.
