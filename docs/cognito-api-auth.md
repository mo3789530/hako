# Cognito access-token authentication for Hako API

## Backend middleware

`internal/auth.NewCognitoVerifier` verifies Cognito access JWTs. `internal/api.NewHandler` builds the Echo v5 router and applies native Echo middleware for authentication and route scopes. `GET /healthz` is a minimal public liveness endpoint for load balancers; it returns only `{"status":"ok"}`. `GET /v1/health` remains protected by access-token verification and the `hako/api` scope. Missing/invalid credentials return 401; a valid token without the required scope returns 403 with an `insufficient_scope` bearer challenge.

API errors use a stable JSON envelope: `{"error":{"code":"...","message":"..."}}`. Internal errors are not returned to clients; unexpected failures become generic HTTP 500 responses.

Validation requires:

- `RS256` signature verified against the RSA public key selected by JWT `kid` from the configured user-pool JWKS.
- Exact configured `iss` and expected app-client `client_id`.
- `token_use=access` (ID tokens are rejected), non-empty `sub`, and required, valid `exp`/`iat` claims.
- Optional resource-bound `aud` to match the configured API audience. An unexpected `aud` is rejected.

JWKS keys are cached for 15 minutes by default. A missing `kid` triggers a refresh at most once per minute to bound unknown-key fetches. Signing-key service failures fail closed with HTTP 503; invalid or missing credentials return HTTP 401. Tenant membership and Workspace RBAC remain Hako database responsibilities. The platform-only Resource Plane Health management routes additionally require the verified Cognito `hako-admin` group; that group is not a Tenant role. See [Resource Plane Health](resource-plane-health.md).

JWT verification is offline after JWKS retrieval and cannot detect token revocation before `exp`. Keep access-token lifetime appropriately short; API request authorization must never rely only on a decoded, unverified JWT.

## API Gateway HTTP API authorizer

The Control Plane Terraform module defines an API Gateway HTTP API, Cognito JWT authorizer, the `hako/api` custom scope, and a public Hako CLI app client on a supplied existing Cognito User Pool. `GET /healthz` explicitly disables API Gateway authorization; the `$default` route requires JWT authorization and `hako/api` scope for all other paths, including `/v1/health`. The dev root requires the exact `cognito_issuer_url` and `cognito_user_pool_id`; it does not create the User Pool or its domain. The Terraform-managed app client permits `openid email profile hako/api`, uses authorization-code flow without a client secret, and registers the configured callback URLs. The JWT authorizer audience is tied directly to this app client. The root outputs the API ID, authorizer ID, scope identifier, and client ID. Ensure the pool ID in `cognito_issuer_url` matches `cognito_user_pool_id`.

The Go API handler is runnable locally and Terraform defines API Gateway routes, Lambda integration, and stage. A real deployment still requires operator-provided Cognito resources, DSQL role bootstrap/migrations, and reviewed AWS applies. Configure each protected route with:

- `issuer`: the exact Cognito User Pool issuer, typically `https://cognito-idp.<region>.amazonaws.com/<userPoolId>`.
- `audience`: the public CLI app-client ID.
- `identitySource`: `$request.header.Authorization`.
- route `authorizationScopes`: require `hako/api` on each protected route.

The CLI requests `openid email profile hako/api` by default and stores tokens in the OS credential store. It can call the protected health route with `hako api health`, query membership with `hako tenant membership <tenant-id>`, submit Workspace creation with `hako create <tenant-id> <name>`, and request suspend/resume/delete. Lifecycle requests persist an Operation and Outbox command; the process Dispatcher or opt-in scheduled Lambda delivers it, and the Fake Resource Controller can exercise the asynchronous result path. This does not yet start or stop a real MicroVM: the production Runtime adapter is still pending. See [Tenant identity/RBAC](tenant-identity-and-rbac.md), [Workspace create flow](workspace-create-flow.md), [Workspace lifecycle API](workspace-lifecycle-api.md), and [Outbox Dispatcher](outbox-dispatcher.md). Terraform defines the API Gateway route/integration, but a real deployment still requires operator-provided Cognito resources, DSQL role bootstrap/migrations, and reviewed AWS applies.

API Gateway can otherwise treat an ID token as a JWT that passes basic issuer/audience checks. A route scope distinguishes access-token authorization; Hako backend middleware independently enforces `token_use=access`. API Gateway and backend validation are defense in depth, not a replacement for Hako RBAC. AWS documents the HTTP API JWT authorizer claim checks and recommends route scopes to distinguish access tokens from other JWTs in the [HTTP API JWT authorizer guide](https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-jwt-authorizer.html).
