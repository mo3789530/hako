# API Gateway HTTP API and Go Lambda

The Control Plane Terraform module routes the HTTP API `$default` route to the Hako API Lambda using API Gateway payload format `2.0`. The route requires the Cognito JWT authorizer and the `hako/api` scope. The Lambda permission is restricted to invocation from that API. API Gateway scope validation is an outer check; `cmd/hako-api` still validates the Cognito token and scope itself.

## Lambda runtime adapter

`cmd/hako-api` detects the Lambda Runtime API environment and starts the AWS Go Lambda runtime instead of listening on a TCP port. `internal/apigwlambda` adapts API Gateway v2 events to the existing `net/http`/Echo handler and maps status, headers, cookies, and body back to the v2 proxy response. Requests larger than 10 MiB receive HTTP 413. Local development remains an ordinary HTTP server.

Build an arm64 custom-runtime package with:

```sh
make build-api-lambda
```

This creates `build/hako-api.zip` with the executable named `bootstrap`, for Lambda's `provided.al2023` runtime and arm64 architecture. The Lambda function itself is supplied to this module as `api_lambda_arn`; a later Resource Plane/Control Plane infrastructure task will own the function, role, DSQL connectivity, logs, and deployment artifact lifecycle.

## Terraform inputs

The dev Control Plane root requires `api_lambda_arn` in addition to Cognito values. The Lambda must be configured with `HAKO_COGNITO_ISSUER`, `HAKO_COGNITO_CLIENT_ID`, `HAKO_DEFAULT_WORKSPACE_IMAGE`, and the appropriate Aurora DSQL connection settings. Its execution role needs CloudWatch Logs permissions and `dsql:DbConnect` scoped to the Control Plane DSQL cluster. Do not put database passwords or AWS credentials in environment variables.

API Gateway sends the HTTP API v2 proxy event and expects its v2 proxy response shape; route scopes can require OAuth scopes before invoking the integration. See [AWS Lambda proxy integration](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-develop-integrations-lambda.html), [HTTP API JWT authorization](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-jwt-authorizer.html), and [Go Lambda handlers](https://docs.aws.amazon.com/lambda/latest/dg/golang-handler.html).

No AWS resource has been applied by this task. Applying the dev root requires a deployed Lambda ARN and the separately configured Cognito/DSQL resources.
