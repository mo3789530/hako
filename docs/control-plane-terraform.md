# Control Plane Terraform

The Control Plane Terraform module provisions an HTTP API Gateway, Cognito JWT authorizer and public CLI app client, the API Lambda, Aurora DSQL, scoped IAM policies, and API Lambda log retention. The dev root expects a Cognito User Pool to already exist; this prevents this module from silently taking ownership of an account-wide identity resource.

The API Lambda connects using the custom DSQL role `hako_api` and the `dsql:DbConnect` IAM action. A separate customer-managed policy grants `dsql:DbConnectAdmin` on the same cluster for database bootstrap/migration principals only. Do not attach that policy to the API Lambda role. Aurora DSQL maps a custom database role to an IAM role with `AWS IAM GRANT`; Terraform cannot create that database-side mapping through the AWS provider.

## Local preparation

From the repository root:

```sh
make build-api-lambda
make build-dispatcher-lambda # only when enable_outbox_dispatcher = true
make build-webhook-processor-lambda # only when enable_github_webhook_processor = true
tofu -chdir=infra/terraform/environments/dev/control-plane init
tofu -chdir=infra/terraform/environments/dev/control-plane plan \
  -var='cognito_issuer_url=https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example' \
  -var='cognito_user_pool_id=ap-northeast-1_Example'
```

The ZIP path defaults to `build/hako-api.zip`. `plan` and `validate` are safe local checks; applying creates billable AWS resources and must only be done in the designated Control Plane account after reviewing the plan. The DSQL cluster has deletion protection enabled by default. Keep it enabled for shared or production environments.

The optional API container image uses `make build-api-lambda-image` and AWS Lambda Web Adapter. Terraform creates the ECR repository and exports its URL. ECR publishing is a separate authorized release step; set `api_lambda_image_uri` to a pushed digest while `api_lambda_image_active=false` to create a candidate without changing production traffic. After validating it with a direct Lambda invoke, a separately reviewed apply with `api_lambda_image_active=true` promotes it. Set that flag back to `false` to roll back to ZIP. See [API Gateway and Lambda](api-gateway-lambda.md).

## Database bootstrap sequence

The first Terraform apply creates the cluster and API Lambda. Until bootstrap completes, API requests that access DSQL will fail; do not route production traffic yet.

1. Attach the module output `dsql_migration_admin_policy_arn` to a dedicated, short-lived migration principal. That principal also needs AWS credentials and network reachability to the DSQL endpoint.
2. Use a DSQL-compatible PostgreSQL client to connect to the output `dsql_endpoint` as database user `admin` with IAM authentication, then run `make migrate` with `HAKO_DSQL_HOST`, `AWS_REGION`, and `HAKO_DSQL_USER=admin` set.
3. Edit `infra/terraform/modules/control-plane/bootstrap-api-role.sql.tmpl`, replacing the API and Dispatcher role ARN placeholders with outputs `api_lambda_role_arn` and `outbox_dispatcher_role_arn`; if enabling the GitHub Webhook Processor, also replace its placeholder with `github_webhook_processor_role_arn`. Run the statements once as `admin` after the schema migrations finish. The API role receives current-table DML grants; the Dispatcher role receives Outbox-only grants; the webhook processor receives delivery/binding read, delivery-status update, and repository-registry DML grants. A newly added table requires a deliberate grant review before deploying code that accesses it.
4. Apply the Resource Plane root with the exact Dispatcher and Result Consumer role ARNs so its command/result queue policies trust those roles.
5. Test a health/API request before sending traffic. For any future migration that adds tables, grant the `hako_api` role the required privileges on those tables before deploying API code that uses them. The bootstrap SQL deliberately does not grant DDL or database-admin privileges.
6. Only after DSQL bootstrap and Resource Plane queues are ready, provide the Resource Plane manifest JSON and set `enable_outbox_dispatcher = true` in the Control Plane root. Apply and verify a successful scheduled invocation before routing production traffic.
7. To enable webhook event processing, set `enable_github_webhook_processor = true` only after its role mapping and migration are ready; verify the processor Logs and Errors alarm.
8. Detach the migration policy from the short-lived principal when migration work is complete.

Aurora DSQL distinguishes `dsql:DbConnectAdmin` for the built-in `admin` role from `dsql:DbConnect` for custom roles, and requires an `AWS IAM GRANT` mapping plus SQL privileges for the custom role. See [Aurora DSQL authentication and authorization](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/authentication-authorization.html) and [database roles with IAM authentication](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/using-database-and-iam-roles.html).

## Boundaries and unfinished infrastructure

The optional Outbox Dispatcher is disabled by default. The initial Control Plane apply creates its stable execution role so Resource Plane queue policies can trust the exact role ARN. Apply Resource Plane queues, run DSQL migrations/bootstrap, then enable the schedule with the versioned manifest. The module still does not provision the Cognito User Pool, Scheduler, Reconciler, Result Consumer, or Resource Plane queues. It does not execute Terraform apply, SQL bootstrap, or migrations automatically. See [`docs/api-gateway-lambda.md`](api-gateway-lambda.md), [`docs/outbox-dispatcher.md`](outbox-dispatcher.md), and [`docs/development.md`](development.md) for local execution and database configuration.
