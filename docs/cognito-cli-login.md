# Hako CLI Cognito login

## Flow

`hako login` uses Cognito's public native app client and Authorization Code + PKCE (S256).

1. CLI binds a loopback HTTP callback, generates cryptographically random `state` and PKCE `code_verifier`, and computes the S256 challenge.
2. The user's browser opens Cognito `/oauth2/authorize` with `response_type=code`, `state`, callback URL, and `code_challenge`.
3. The CLI accepts a callback only when the returned state matches, then exchanges the one-time code at `/oauth2/token` with the original verifier and exact same redirect URI.
4. The returned access/ID/refresh tokens are stored in the operating system credential manager, not a plaintext config file.

The app client is public: do not configure or embed a client secret. OAuth state mitigates callback CSRF; PKCE binds authorization-code redemption to the CLI invocation. The CLI does not accept AWS credentials. It stores Cognito tokens in the OS credential manager; `hako api health` uses a still-valid access token to call the protected API health endpoint. Hako Session Ticket exchange and refresh-token renewal are not implemented; when the access token expires, run `hako login` again.

## Cognito setup

Configure a Cognito User Pool domain. The Control Plane Terraform root creates the Hako CLI app client in the supplied User Pool with:

- Authorization code grant enabled; implicit grant disabled.
- Public client with no client secret.
- `openid`, `email`, `profile`, and the custom `hako/api` scope allowed.
- Callback URL exactly `http://127.0.0.1:53682/callback` (or the exact override configured for the CLI).
- PKCE using `S256`.

Cognito requires HTTPS except for loopback callback URLs and supports a custom TCP port for localhost callback URLs. The callback URL must be registered in the app client and must match the runtime redirect URI. See the [Cognito authorization endpoint](https://docs.aws.amazon.com/cognito/latest/developerguide/authorization-endpoint.html) and [app client callback URL constraints](https://docs.aws.amazon.com/cli/latest/reference/cognito-idp/create-user-pool-client.html).

Configure and run:

```sh
export HAKO_COGNITO_DOMAIN='https://<your-cognito-domain>.auth.<region>.amazoncognito.com'
export HAKO_COGNITO_CLIENT_ID='<public-app-client-id>'
# Optional; must also be registered in the Cognito app client.
export HAKO_COGNITO_CALLBACK_URL='http://127.0.0.1:53682/callback'
go run ./cmd/hako login
```

The CLI requests `openid email profile hako/api` by default. The Control Plane Terraform module creates the `hako/api` resource-server scope and an app client that allows it. Retrieve the generated client ID with `tofu -chdir=infra/terraform/environments/dev/control-plane output -raw cognito_app_client_id`, then use that value for `HAKO_COGNITO_CLIENT_ID`. The User Pool domain remains separately configured and is supplied as `HAKO_COGNITO_DOMAIN`.

To run and call the local API handler, configure the same issuer and client ID for the API verifier, then use the API origin for the CLI:

```sh
export HAKO_COGNITO_ISSUER='https://cognito-idp.<region>.amazonaws.com/<userPoolId>'
export HAKO_COGNITO_CLIENT_ID='<public-app-client-id>'
export HAKO_API_LISTEN_ADDR='127.0.0.1:8080'
go run ./cmd/hako-api
```

In another terminal, after `hako login`, run:

```sh
export HAKO_API_URL='http://127.0.0.1:8080'
go run ./cmd/hako api health
```

The CLI sends the access token as a bearer credential and prints no token material. `HAKO_API_URL` permits plain HTTP only for loopback development addresses; non-loopback endpoints must use HTTPS.

To submit an asynchronous Workspace create request, configure the tenant and server-side default Placement/Image as described in [development](development.md), then run:

```sh
go run ./cmd/hako create tenant_123 api-dev
```

The CLI generates an `Idempotency-Key` and prints it with the result. If the response is ambiguous, reuse that key for the same request with `--idempotency-key <key>`; a different request must use a new key. A quota overflow is reported as `workspace_quota_exceeded` (HTTP 409); the limit is ten non-deleted Workspaces per Tenant.

On macOS the keychain is used; on Windows the Credential Manager is used; on Linux/BSD the Secret Service D-Bus API must be available (for example GNOME Keyring or KDE Wallet). Headless Linux without a Secret Service provider cannot persist a login and returns an error rather than writing tokens to a file. The keyring entry service/account are `hako-cli` / `default`.

The callback listener binds only to `127.0.0.1`, has a five-minute default timeout, and is closed after login. The authorization code is exchanged over HTTPS and is not printed. The operating-system credential manager contains bearer and refresh tokens, so use the platform's lock and access-control features.
