# GitHub Webhook Processor

The webhook API authenticates and durably records a delivery; it does not run
event side effects on the request path. `cmd/hako-webhook-processor-lambda`
drains supported Installation lifecycle and repository-membership events from
that inbox.

## Processing contract

- A batch is bounded to 10 deliveries by the scheduled Lambda (the store
  supports an explicit maximum of 100).
- Schema-v1 `installation_repositories` events are selected only for active,
  non-suspended Installation-to-Tenant bindings. Installation `created`,
  `deleted`, `suspend`, and `unsuspend` events are processed by installation
  ID. Push, pull request, workflow job, and older schema deliveries stay in
  `received` state for their appropriate future consumer.
- Each delivery's repository sync and `received` → `processed` update share a
  transaction. A retry therefore cannot partially sync a repository or apply
  the same event twice. Concurrent invocations may observe the same candidate;
  the transaction/state checks make processing idempotent.
- Installation events never establish Tenant ownership. `created` is an
  authorization-neutral no-op; `suspend` blocks active Repository access,
  `unsuspend` restores access only for an already-active claim, and `deleted`
  revokes the claim and deletes its synchronized Repository rows. An unbound
  Installation lifecycle event is acknowledged as processed without creating
  a claim or binding. The global Installation binding is retained after
  revocation to prevent automatic reassignment to a different Tenant; operator
  reassignment tooling is not implemented.
- A database error fails the Lambda invocation so EventBridge retries. The
  durable inbox remains the source of truth. Unbound repository-membership
  events are not selected; unbound lifecycle events are processed as no-ops,
  so neither causes a hot retry loop.

## Local verification

The integration suite uses isolated PostgreSQL schemas and exercises
active-binding selection, lifecycle transitions, idempotency, and pending
events:

```sh
HAKO_TEST_DATABASE_URL='postgres://…' go test -tags=integration ./internal/store/githubwebhook ./internal/store/githubregistry -count=1
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
   and apply its table-level grants as the migration admin.
4. Build the ZIP with `make build-webhook-processor-lambda`, set
   `enable_github_webhook_processor=true`, and apply the reviewed plan.
5. Confirm successful scheduled invocations and monitor the processor Errors
   alarm before relying on automatic delivery processing.

The runtime IAM role has only CloudWatch log-write and `dsql:DbConnect` access
to the Control Plane cluster. It has no Secrets Manager, GitHub API, SQS, or
schema-admin permission. SQL table grants are limited to delivery status,
Installation lifecycle fields, binding lookup, and Tenant repository registry
synchronization/deletion.
Never enable the schedule before applying the migration and custom DSQL role
mapping. No Terraform apply is performed by local development or CI.

## Remaining work

This processor does not verify GitHub's setup callback or activate pending
Installation claims. It also does not process push, pull-request, or workflow
job events, call GitHub APIs, create Check Runs, schedule workloads, or expose
manual replay/retention tooling. Those consumers must preserve the same
tenant-binding authorization and idempotent transaction boundaries.
