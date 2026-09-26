# Control Plane Terraform module

Creates a Cognito-protected HTTP API, an arm64 API Lambda, an encrypted Aurora DSQL cluster, scoped IAM permissions, and a separate migration-admin policy. It also creates a stable least-privilege Outbox Dispatcher role by default, with an optional scheduled arm64 Dispatcher Lambda.

It does not create the Cognito User Pool, run SQL migrations, create Resource Plane messaging/compute, or deploy the Scheduler/Reconciler/Result Consumer. The Dispatcher publishes directly to Resource Plane command queues; there is no separate Control Plane queue.

Build the API package with `make build-api-lambda` before planning/applying. If enabling the scheduled Dispatcher, also run `make build-dispatcher-lambda`, configure `enable_outbox_dispatcher` and the versioned Resource Plane manifest, and follow [`docs/control-plane-terraform.md`](../../../../docs/control-plane-terraform.md) for the safe deployment order. SQL role bootstrap and migrations are operator-run. The API Lambda uses `dsql:DbConnect` only; the API role must not receive the separate `dsql:DbConnectAdmin` migration policy. The Dispatcher has its own `dsql:DbConnect` role and is limited to reading/updating Outbox rows and sending to manifest-listed command queues.

The module also creates a private ECR repository for API images. Setting `api_lambda_image_uri` to a digest creates a distinct image Lambda candidate but leaves ZIP active. After candidate validation, set `api_lambda_image_active=true` to switch API Gateway; set it back to `false` to roll back while retaining the candidate. Build with `make build-api-lambda-image`, but publish only from an authorized release environment. The image uses AWS Lambda Web Adapter; see [`docs/api-gateway-lambda.md`](../../../../docs/api-gateway-lambda.md) for local HTTP smoke testing, deployment, and rollback.

The dev environment expects Cognito User Pool inputs managed outside this module. DSQL deletion protection is enabled by default. No AWS apply is run as part of local development or CI validation.

See [`docs/terraform-modules.md`](../../../../docs/terraform-modules.md) for the complete input/output inventory, resource tags, IAM boundaries, and dev environment differences.
