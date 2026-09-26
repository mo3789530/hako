# GitHub Webhook Processor

The webhook API authenticates and durably records a delivery; it does not run
event side effects on the request path. `cmd/hako-webhook-processor-lambda`
drains the supported repository-membership subset from that inbox.

## Processing contract

- A batch is bounded to 10 deliveries by the scheduled Lambda (the store
  supports an explicit maximum of 100).
- Only schema-v1 `installation_repositories` events with an active
  Installation-to-Tenant binding are selected. Push, pull request, workflow
  job, inactive/unclaimed Installation, and older schema deliveries stay in
  `received` state for their appropriate future consumer.
- Each delivery's repository sync and `received` → `processed` update share a
  transaction. A retry therefore cannot partially sync a repository or apply
  the same event twice. Concurrent invocations may observe the same candidate;
  the transaction/state checks make processing idempotent.
- A database error fails the Lambda invocation so EventBridge retries. The
  durable inbox remains the source of truth. Unbound events are not selected,
  so they do not cause a hot retry loop.

## Local verification

The ordinary integration suite uses a disposable PostgreSQL database and
exercises active-binding selection, idempotency, and pending events:

```sh
HAKO_TEST_DATABASE_URL='postgres://…' go test -tags=integration ./internal/store/githubwebhook -count=1
make build-webhook-processor-lambda
```

The always-on local development database is not needed by this test; use the
project's disposable test database configuration. The scheduled Lambda reads
`HAKO_DSQL_HOST`, `HAKO_DSQL_USER`, and `HAKO_DSQL_DATABASE`; locally it also
accepts `HAKO_DATABASE_URL`.

## AWS enablement

The Terraform integration is opt-in and defaults off. Before enabling it:

1. Apply the Control Plane Terraform to create the stable processor IAM role.
2. Run DSQL migrations, including migration `000041`.
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
Installation binding lookup, and Tenant repository registry synchronization.
Never enable the schedule before applying the migration and custom DSQL role
mapping. No Terraform apply is performed by local development or CI.

## Remaining work

This processor does not verify GitHub's setup callback or activate pending
Installation claims. It also does not process push, pull-request, or workflow
job events, call GitHub APIs, create Check Runs, schedule workloads, or expose
manual replay/retention tooling. Those consumers must preserve the same
tenant-binding authorization and idempotent transaction boundaries.
