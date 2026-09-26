# API Gateway HTTP API and Go Lambda

The Control Plane Terraform module routes the HTTP API `$default` route to the Hako API Lambda using API Gateway payload format `2.0`; it requires the Cognito JWT authorizer and `hako/api` scope. The only unauthenticated route is `GET /healthz`, which returns minimal liveness data. `/v1/health` remains authenticated. The Lambda permission is restricted to invocation from that API. API Gateway scope validation is an outer check; `cmd/hako-api` still validates the Cognito token and scope itself.

## ZIP runtime adapter

`cmd/hako-api` detects the Lambda Runtime API environment and starts the AWS Go Lambda runtime instead of listening on a TCP port. `internal/apigwlambda` adapts API Gateway v2 events to the existing `net/http`/Echo handler and maps status, headers, cookies, and body back to the v2 proxy response. Requests larger than 10 MiB receive HTTP 413. Local development remains an ordinary HTTP server.

Build an arm64 custom-runtime package with:

```sh
make build-api-lambda
```

This creates `build/hako-api.zip` with the executable named `bootstrap`, for Lambda's `provided.al2023` runtime and arm64 architecture. The Control Plane Terraform module consumes that artifact and owns the function, role, DSQL connectivity, logs, and deployment lifecycle.

## Terraform inputs

The dev Control Plane root requires Cognito issuer and User Pool values and the built ZIP. The Terraform-managed Lambda receives `HAKO_COGNITO_ISSUER`, `HAKO_COGNITO_CLIENT_ID`, `HAKO_DEFAULT_WORKSPACE_IMAGE`, and Aurora DSQL endpoint settings. Its execution role needs CloudWatch Logs permissions and `dsql:DbConnect` scoped to the Control Plane DSQL cluster. It connects as the custom `hako_api` database role; a one-time database-side IAM mapping and SQL grants are required before serving requests. Do not put database passwords or AWS credentials in environment variables. See [Control Plane Terraform](control-plane-terraform.md).

API Gateway sends the HTTP API v2 proxy event and expects its v2 proxy response shape; route scopes can require OAuth scopes before invoking the integration. See [AWS Lambda proxy integration](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-develop-integrations-lambda.html), [HTTP API JWT authorization](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-jwt-authorizer.html), and [Go Lambda handlers](https://docs.aws.amazon.com/lambda/latest/dg/golang-handler.html).

No AWS resource has been applied. Applying the dev root creates billable resources and must be reviewed before execution in the designated Control Plane account.

## Docker/OCI image deployment

The ZIP deployment remains the default. `cmd/hako-api/Dockerfile` builds a
static Go binary in a multi-stage build and copies the version-pinned AWS
Lambda Web Adapter 1.1.0 into `/opt/extensions`. The OCI image runs the API as
an ordinary HTTP server (`HAKO_API_HTTP_MODE=true`); the ZIP deployment still
uses the Go Lambda runtime and `internal/apigwlambda`. Lambda Web Adapter
converts API Gateway HTTP API v2 events to HTTP requests in Lambda and needs no
adapter-specific Go handler code. Its traffic port is 8080, and readiness
uses the minimal public `/healthz` endpoint; application health details remain
protected under `/v1/health`.

Build a single arm64 image locally with:

```sh
make build-api-lambda-image
```

This uses Docker Buildx with `--platform linux/arm64 --provenance=false` and
tags the result `hako-api:local`. The Lambda Web Adapter is an extension and
does not proxy Lambda events when the image is run as an ordinary local
container. Smoke-test the HTTP app directly against a disposable PostgreSQL
database (the API connects during startup):

```sh
docker run --rm -p 8080:8080 \
  --add-host=host.docker.internal:host-gateway \
  -e HAKO_DATABASE_URL='postgres://hako:local-dev-only@host.docker.internal:5432/hako?sslmode=disable' \
  -e HAKO_COGNITO_ISSUER='https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example' \
  -e HAKO_COGNITO_CLIENT_ID='local-test' hako-api:local
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/v1/health
```

On Podman, replace `host.docker.internal` with the Podman host's reachable
address, or attach both containers to one disposable network.
Unauthenticated `GET /healthz` should return 200 and `/v1/health` should return
401. This proves the HTTP server is reachable, not that API Gateway event
conversion works. For a pushed candidate,
first configure `api_lambda_image_uri` while keeping
`api_lambda_image_active=false`; Terraform creates the image Lambda but leaves
the API Gateway integration pointed at ZIP. Directly invoke the candidate with
a representative API Gateway v2 event (including a short-lived Cognito access
token in the `authorization` header) and verify status, body, auth scope,
headers, and DSQL behavior before promotion. Then set
`api_lambda_image_active=true`; the reviewed Terraform plan should switch only
the integration/permission to the previously tested candidate. Local direct
HTTP smoke and the existing adapter unit/integration tests complement, but do
not replace, this AWS event-conversion check.

Terraform creates a private same-Region ECR repository with immutable tags,
scan-on-push, AES-256 encryption, Lambda-scoped image retrieval permission,
and lifecycle cleanup for untagged artifacts after seven days. Tagged releases
are deliberately not expired automatically: deleting a digest still referenced
by a Lambda or held for rollback can break future execution-environment starts.
Remove tagged images only after checking all Lambda versions and the rollback
retention window. The repository URL is exported as
`api_lambda_ecr_repository_url`. Publishing remains a separately authorized
release step; local builds do not use AWS credentials. Push a unique commit
tag and pass its resolved digest as
`api_lambda_image_uri="<repository>@sha256:<64 hex characters>"`. First apply
with the digest and `api_lambda_image_active=false` to create the candidate
while preserving ZIP traffic, then validate it, and only then apply with
`api_lambda_image_active=true` to promote. The module rejects a URI outside
its own repository and does not accept a mutable tag. Build one architecture
per image and enforce Lambda's 10 GB uncompressed image limit. Never add a
Docker daemon or container-build privilege inside the API Lambda.

Migration must be blue/green: Lambda does not let an existing function switch
between ZIP and container-image package types. Terraform keeps the ZIP Lambda
deployed, creates a distinct `-api-image` candidate when a digest is set, and
only switches API Gateway when `api_lambda_image_active=true`. Roll back by
setting it to `false` and applying the reviewed plan; retain the candidate and
ECR digest for diagnosis until the observation window ends. Never flip the
existing ZIP function's package type in place.

References: [AWS Lambda Web Adapter](https://github.com/aws/aws-lambda-web-adapter),
[Deploy Go Lambda functions with container images](https://docs.aws.amazon.com/lambda/latest/dg/go-image.html),
[Create a Lambda function using a container image](https://docs.aws.amazon.com/lambda/latest/dg/images-create.html).
