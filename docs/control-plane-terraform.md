# Control Plane Terraform

The Control Plane Terraform module provisions an HTTP API Gateway, Cognito JWT authorizer and public CLI app client, the API Lambda, Aurora DSQL, scoped IAM policies, and API Lambda log retention. The dev root expects a Cognito User Pool to already exist; this prevents this module from silently taking ownership of an account-wide identity resource.

The API Lambda connects using the custom DSQL role `hako_api` and the `dsql:DbConnect` IAM action. A separate customer-managed policy grants `dsql:DbConnectAdmin` on the same cluster for database bootstrap/migration principals only. Do not attach that policy to the API Lambda role. Aurora DSQL maps a custom database role to an IAM role with `AWS IAM GRANT`; Terraform cannot create that database-side mapping through the AWS provider.

## Local preparation

From the repository root:

```sh
make build-api-lambda
make build-dispatcher-lambda # only when enable_outbox_dispatcher = true
make build-webhook-processor-lambda # only when enable_github_webhook_processor = true
make build-workspace-idle-reconciler-lambda # only when enable_workspace_idle_reconciler = true
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
3. Edit `infra/terraform/modules/control-plane/bootstrap-api-role.sql.tmpl`, replacing the API, Dispatcher, and stable Checks task-role ARN placeholders with outputs `api_lambda_role_arn`, `outbox_dispatcher_role_arn`, and `github_checks_consumer_task_role_arn`; if enabling the GitHub Webhook Processor, also replace its placeholder with `github_webhook_processor_role_arn`. Replace the Workspace idle reconciler placeholder with the stable `workspace_idle_reconciler_role_arn` output even when its schedule is disabled, then run the statements once as `admin` after the schema migrations finish. The template also contains a `hako_github_broker` role placeholder: replace it only when the separately deployed, CP-only broker has an exact IAM role; no broker Terraform role/output is provisioned by this module today. The Checks ECS task role is created without workload permissions when delivery is disabled; its exact SQS/DSQL IAM policies attach only when `enable_github_checks_delivery=true`. The API role receives current-table DML grants; the Dispatcher role receives Outbox-only grants; the webhook processor receives delivery/integration/Installation binding reads, delivery-status update, repository-registry DML and Actions Runner admission/Check writes; the Checks consumer reads Run and Runner Check state and updates only persisted Check Run IDs; the GitHub Broker reads Tenant-pinned App and Software Catalog policy metadata and can mutate only Actions Runner lifecycle/Check rows. The idle reconciler DSQL role can inspect Workspace policy/runtime/lease state and insert only suspend Operations/events/Outbox/audit while updating the Workspace Desired State fence. A newly added table requires a deliberate grant review before deploying code that accesses it. If the bootstrap ran before Task 54 migration 000138, apply the Software Catalog `SELECT, INSERT, UPDATE` grant and `DELETE` revoke from the updated template as `admin` after applying that migration.
4. Apply the Resource Plane root with the exact Dispatcher and Result Consumer role ARNs so its command/result queue policies trust those roles.
5. Test a health/API request before sending traffic. For any future migration that adds tables, grant the `hako_api` role the required privileges on those tables before deploying API code that uses them. The bootstrap SQL deliberately does not grant DDL or database-admin privileges.
6. Only after DSQL bootstrap and Resource Plane queues are ready, provide the Resource Plane manifest JSON and set `enable_outbox_dispatcher = true` in the Control Plane root. Apply and verify a successful scheduled invocation before routing production traffic.
7. To enable webhook event processing, set `enable_github_webhook_processor = true` only after its role mapping and migration are ready; verify the processor Logs and Errors alarm.
8. To enable Tenant GitHub App setup, configure both the App Setup URL and OAuth callback URL as `https://<api-host>/v1/integrations/github/setup/callback`, store the OAuth client secret in Secrets Manager, provide the App settings, and set `github_app_setup_enabled = true`. Terraform grants the API role access only to that secret ARN. Setup enablement also requires migration `000043`; grant `hako_api` DML on `github_installation_setup_states` using the bootstrap template.
9. Tenant-owned Webhook ingress is separately default-off. Before setting `tenant_github_webhook_enabled = true`, apply migrations `000060`–`000061` and `000064`, provision each approved Webhook-purpose reader Role and exact Secret version out of band, add only those Role ARNs to `tenant_github_webhook_reader_role_arns`, and ensure the role trust policies name the API Lambda role. The API policy permits `sts:AssumeRole` only on that enumerated set; the route is `POST /v1/integrations/github/apps/{integration_id}/webhook` and does not use Cognito. API Gateway applies a per-route default of 50 requests/second and burst 100; both limits are configurable. The API also enforces a DSQL-backed 120 attempts/minute budget per active integration (`HAKO_TENANT_GITHUB_WEBHOOK_RATE_LIMIT` overrides the API default). Malformed headers and oversized requests are rejected before Secret retrieval; exhausted budgets return 429 with `Retry-After: 60`. No private-key or OAuth reader Role is accepted by the Terraform variable validation. Installation App-ID consistency is checked when present. Keep this feature disabled until integration-aware OAuth setup and role/secret provisioning are complete. This is configuration/validation only; do not apply without the normal review and approval.

10. Tenant-owned GitHub private-key reader Roles use a distinct principal and role path. Configure the existing broker task role with `github_broker_role_name`, then list only operator-approved exact ARNs in `tenant_github_private_key_reader_role_arns`. The Control Plane module attaches an inline policy containing only `sts:AssumeRole` to those ARNs; Terraform rejects wildcard/wrong-purpose paths and overlap with the API Webhook reader set. Each target cross-account Role must trust only the broker role and allow `secretsmanager:GetSecretValue` for its exact approved private-key Secret/version, with narrowly scoped KMS decrypt only where needed. This module does not create target trust policies or Secrets. Apply this boundary only after reviewing the broker role identity and every target Role/Secret/KMS policy. `make terraform-test` includes mock-plan tests; they do not retrieve Secret values or apply resources.
10. Detach the migration policy from the short-lived principal when migration work is complete.

11. To deploy automatic Workspace idle suspension, build the Lambda ZIP and
    first apply with `enable_workspace_idle_reconciler=false` so the stable
    execution-role ARN is available. Complete migrations and the role mapping
    above before enabling the schedule. Then set
    `enable_workspace_idle_reconciler=true`; the module attaches DSQL `DbConnect`,
    creates a one-minute default EventBridge schedule and Lambda error alarm.
    The Tenant policy still defaults to disabled; enable it per Tenant through
    the Owner/Admin API or CLI only after reviewing the 5-minute minimum grace
    and local-disk loss behavior. Terraform code is default-off and has not been
    applied to AWS.

Aurora DSQL distinguishes `dsql:DbConnectAdmin` for the built-in `admin` role from `dsql:DbConnect` for custom roles, and requires an `AWS IAM GRANT` mapping plus SQL privileges for the custom role. See [Aurora DSQL authentication and authorization](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/authentication-authorization.html) and [database roles with IAM authentication](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/using-database-and-iam-roles.html).

## Boundaries and unfinished infrastructure

The optional Outbox Dispatcher is disabled by default. The initial Control Plane apply creates its stable execution role so Resource Plane queue policies can trust the exact role ARN. Apply Resource Plane queues, run DSQL migrations/bootstrap, then enable the schedule with the versioned manifest. GitHub Checks delivery is separately disabled by default; `enable_github_checks_delivery = true` requires the Dispatcher and creates a dedicated encrypted queue/DLQ, send-only Dispatcher permission, a dedicated ECS/Fargate task role with only queue receive and DSQL connect policies, and queue age/depth/DLQ alarms. Optional `github_checks_alarm_actions` SNS topics receive alarm and OK actions. `enable_github_checks_consumer_service` separately opts into an ARM64 Fargate service and task definition with private subnet attachment, digest-pinned image, separate Secrets Manager execution role, and exact broker mTLS Secret ARNs. It requires a pre-created ECS cluster and security/network configuration. The Workspace idle reconciler is default-off and uses its own DSQL role; schema migrations and SQL grants must precede schedule enablement. Terraform references Secret ARNs but does not retrieve their values; Secret creation/rotation, SQL role mapping/grants and AWS apply remain operator-controlled. Do not enable Checks delivery until the consumer service, its mTLS broker path, and the GitHub App permission are reviewed. The module still does not provision the Cognito User Pool, Workload Scheduler consumer, Result Consumer runtime, or Resource Plane queues. It does not execute Terraform apply, SQL bootstrap, or migrations automatically. See [`docs/api-gateway-lambda.md`](api-gateway-lambda.md), [`docs/outbox-dispatcher.md`](outbox-dispatcher.md), and [`docs/development.md`](development.md) for local execution and database configuration.

For Workload Artifact expiry, provision a dedicated cleanup HMAC Secret and
configure its exact ARN for the Resource Plane Controller and the matching
entry in `resource_plane_artifact_cleanup_secret_arns` here. If the Secret is
cross-account, its resource policy must allow only the Control Plane API role
and the exact Resource Plane Worker role. Keep the Resource Plane cleanup
schedule default-off until the bucket's versioning/lifecycle behavior and
object-prefix IAM have been reviewed; the Control Plane API Lambda only lists
candidates and acknowledges completed deletes, and never accesses S3.

## Monitoring alarms

The Control Plane module creates API Gateway stage alarms for server errors
(more than five 5xx responses in a minute) and p95 end-to-end latency (over
two seconds for five consecutive minutes). It also alarms on Aurora DSQL query
timeouts and sustained OCC conflicts (more than ten per minute for five
consecutive minutes). The API alarms use the HTTP API `ApiId` and `Stage`
dimensions; DSQL alarms use the cluster's `ClusterId` dimension. Thresholds are
initial operating defaults and should be tuned from observed traffic before
setting an SLO. Missing metrics are treated as non-breaching, so these alarms
do not confirm that a service is deployed or emitting telemetry. Alarm actions
are intentionally not configured by the module; attach notifications in the
environment after reviewing the destination and escalation policy.

The Outbox Dispatcher emits the oldest unpublished event age as a low-cardinality
CloudWatch Embedded Metric Format log record on each scheduled invocation. A
partial index keeps the lookup bounded to the oldest pending row; it does not
count or scan the full Outbox table. When the Dispatcher is enabled, Terraform
alarms if this age exceeds five minutes for two consecutive periods. The
Dispatcher invocation-error and queue/DLQ alarms remain separate. See [HTTP API
CloudWatch metrics](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-metrics.html)
and [Aurora DSQL CloudWatch metrics](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/cloudwatch-monitoring.html)
for the AWS metric names and dimensions used here.
