# Control Plane Terraform module

Creates a Cognito-protected HTTP API, an arm64 API Lambda, an encrypted Aurora DSQL cluster, scoped IAM permissions, and a separate migration-admin policy. It also creates a stable least-privilege Outbox Dispatcher role by default, with an optional scheduled arm64 Dispatcher Lambda.

It does not create the Cognito User Pool, run SQL migrations, create Resource Plane messaging/compute, or deploy the Scheduler/Reconciler/Result Consumer. The Dispatcher publishes directly to Resource Plane command queues; the optional Checks queue is a separate Control Plane result-delivery queue. The optional `enable_github_checks_delivery` (default `false`) creates a dedicated encrypted Checks queue/DLQ, a send-only Dispatcher grant, an ECS/Fargate task role with receive-only queue and DSQL-connect permissions, and queue age/depth/DLQ alarms. Optional `github_checks_alarm_actions` SNS topics receive alarm/OK actions. The separate `enable_github_checks_consumer_service` (default `false`) optionally deploys the ARM64 Fargate service/task definition, private-subnet network attachment, restricted execution/task roles, and exact Secrets Manager references for broker mTLS. It requires a pre-created ECS cluster, network/security groups, a digest-pinned ECR image whose account/Region/repository must match the exact repository ARN allowed by the task execution role, broker URL, and operator-provisioned Secrets.

Build the API package with `make build-api-lambda` before planning/applying. If enabling the scheduled Dispatcher, also run `make build-dispatcher-lambda`, configure `enable_outbox_dispatcher` and the versioned Resource Plane manifest. Checks delivery additionally requires `enable_github_checks_delivery=true`, which is validated to require the Dispatcher; the module creates a dedicated ECS task role and exports `github_checks_consumer_task_role_arn`. Configure optional SNS notifications through `github_checks_alarm_actions`. If enabling the GitHub Webhook Processor, run `make build-webhook-processor-lambda` and set `enable_github_webhook_processor`. Automatic Workspace idle suspension is independently opt-in through `enable_workspace_idle_reconciler` (default `false`); build `make build-workspace-idle-reconciler-lambda` before enabling it. The stable `workspace_idle_reconciler_role_arn` exists while disabled so an operator can map the dedicated `hako_workspace_idle_reconciler` DSQL role and apply its table grants before activating the one-minute default schedule. The Lambda only creates normal suspend Operations and never calls Resource Plane/provider APIs. Tenant policy is separately disabled by default. To enable tenant GitHub App setup, configure the App slug/ID/client ID, HTTPS OAuth callback URL, and client-secret ARN, then set `github_app_setup_enabled=true`; see [`docs/github-installation-setup.md`](../../../../docs/github-installation-setup.md). Follow [`docs/control-plane-terraform.md`](../../../../docs/control-plane-terraform.md) for the safe deployment order. SQL role bootstrap and migrations are operator-run. The API Lambda uses `dsql:DbConnect` only; the API role must not receive the separate `dsql:DbConnectAdmin` migration policy. The Dispatcher, Webhook Processor, idle reconciler, and Checks consumer have separate least-privilege DSQL roles; the Dispatcher also sends to manifest-listed command queues and, when enabled, the Checks queue.

The module also creates a private ECR repository for API images. Setting `api_lambda_image_uri` to a digest creates a distinct image Lambda candidate but leaves ZIP active. After candidate validation, set `api_lambda_image_active=true` to switch API Gateway; set it back to `false` to roll back while retaining the candidate. Build with `make build-api-lambda-image`, but publish only from an authorized release environment. The image uses AWS Lambda Web Adapter; see [`docs/api-gateway-lambda.md`](../../../../docs/api-gateway-lambda.md) for local HTTP smoke testing, deployment, and rollback.

The dev environment expects Cognito User Pool inputs managed outside this module. DSQL deletion protection is enabled by default. No AWS apply is run as part of local development or CI validation.

Preview expiry metadata reconciliation is independently opt-in through `enable_preview_expiry_reconciler` (default `false`). The stable `preview_expiry_reconciler_role_arn` allows mapping the dedicated `hako_preview_reconciler` DSQL role before enabling it. When enabled, the module requires the ARM64 ZIP from `make build-preview-expiry-lambda`, grants only DSQL `DbConnect` and scoped log writes, and creates an EventBridge schedule (default `rate(1 minute)`) plus a Lambda Errors alarm. The batch is bounded to 1–500. Its SQL grants allow expiry-key reads and cleanup metadata updates only; it does not delete Preview records or operate MicroVMs, EFS, or Gateway routes. The feature remains disabled by default and has not been applied.

The optional `resource_plane_artifact_cleanup_secret_arns` map enables the
HMAC-authenticated artifact cleanup API for exact Resource Plane IDs. Each
entry references the same dedicated cleanup HMAC Secret configured on that
Resource Plane's Workload Controller; its value must not be reused for health
reports or execution metadata. The API role receives `GetSecretValue` only for
the listed ARNs. For cross-account Secrets, an operator-managed resource
policy must grant this exact API role access; this module does not create or
retrieve the Secret.

Tenant-owned GitHub App private keys use a different identity boundary from
Webhook secrets. Set `github_broker_role_name` and the exact
`tenant_github_private_key_reader_role_arns` allowlist to attach a policy that
grants only `sts:AssumeRole` on those Tenant reader Roles to the existing
`hako-github-broker*` role. The API Lambda's Webhook-reader allowlist is kept
separate, and Terraform validation rejects overlap between the two sets. Empty
private-key reader input creates no broker policy. The module does not create
cross-account reader-role trust policies, Tenant Secrets, or KMS grants; those
must be provisioned and reviewed separately. See the
[`github-broker-tenant-iam` module](../github-broker-tenant-iam/README.md).

See [`docs/terraform-modules.md`](../../../../docs/terraform-modules.md) for the complete input/output inventory, resource tags, IAM boundaries, and dev environment differences.
