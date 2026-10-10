# GitHub App credentials for read-only Job checkout

Task 37 provides `internal/githubapp.TokenClient` and the trusted Control Plane
`githubregistry.CredentialAuthorizer`. Task 38 adds the worker-side source
checkout package that uses an injected short-lived credential provider. These
are internal libraries, not a public token API or deployed worker. Tasks 39/40
wire and deploy the trusted orchestration path. Run creation remains default-off.

## Configuration and trust boundary

The current configuration is **one Hako GitHub App and one private-key secret
ARN per credential-service instance**, shared across that App's Tenant
Installations. It is not a separate private-key secret for every Tenant.
Verified Installation bindings, repository authorization and token-cache keys
are Tenant-scoped; one Tenant cannot request another Tenant's token. Registering
a Tenant-owned GitHub App with its own App ID/private-key ARN, secret lifecycle
and routing is not implemented; its [design and Tasks 56–60](tenant-github-apps.md)
are now recorded separately. The existing OAuth client secret likewise
belongs to the configured Hako App, not to an individual Tenant.

`TokenClient.VerifyApp` is a trusted-library primitive for verifying a
configured key/App ID pair through GitHub `GET /app`. It returns no App details
or credential material and does not persist a `ready` status. Tenant-owned
verification must still resolve the authoritative integration and pinned
credentials, run this request outside a database transaction, then recheck the
integration revision before recording the result.

The future trusted credential service reads these values through
`TokenConfigFromEnv`:

```text
HAKO_GITHUB_APP_ID=<positive numeric App ID>
HAKO_GITHUB_APP_PRIVATE_KEY_SECRET_ARN=<exact Secrets Manager secret ARN>
```

Store the RSA private key as a raw PEM `SecretString` (PKCS#1 or PKCS#8,
at least 2048 bits), not JSON, a PAT, or the OAuth client secret. Never put the
key/token in Terraform state, DSQL, Outbox/SQS, Run metadata, Job environment,
repository URLs, command arguments, logs, or artifacts. Configure the AWS SDK
with the trusted service's IAM role; do not distribute AWS keys to clients.

Only the CP credential service may combine the DB authorizer and key loader.
Resource Plane executors have neither DSQL access nor the App private-key read
permission. Task 39 must authenticate internal callers and derive Tenant,
Installation and Repository from the authoritative Run, not a supplied webhook
payload. `CheckoutToken` is not authorization for arbitrary user token issuance.

For the initial checkout, grant the GitHub App **Contents: read** and GitHub's
required **Metadata: read**. Installation tokens are requested with exactly one
`repository_ids` entry and `permissions: {contents: read}`. Write access, Checks,
Issues, Actions and runner administration are not added here. The response is
rejected if it includes another repository or any unexpected permission.

The authorizer joins the exact Tenant/repository to its globally bound, active,
non-suspended Installation. The token client additionally checks GitHub's live
Installation App ID/suspension and the repository's current Installation before
minting **and before returning a cached token**. DB authorization is checked
again after remote calls. Denial or remote validation failure evicts that cache
entry. A remote revocation can still occur after these checks; checkout must
fail closed on authentication failure and must not treat a token as persistent
authorization. See [GitHub installation-token guidance](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

## Lifetime, cache and failures

- App JWT uses RS256, `iss` = numeric App ID, `iat` = now minus 60 seconds,
  `exp` = now plus 9 minutes. Keep the trusted host clock synchronized.
  [GitHub JWT guidance](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)
- Read `AWSCURRENT` for each authorized request; private keys are not cached
  across requests. The AWS read has a 10-second deadline. SDK retry behavior is
  bounded by that context and the service's configured SDK retry policy.
- Installation tokens are opaque, not assumed to be 40 characters. Cache only
  in process memory, keyed by Tenant + Installation + Repository, maximum 128
  entries. Refresh at least two minutes before expiry; reject expired or
  implausibly long-lived responses. Cache/HTTP calls are serialized per client,
  with context-aware waits, so concurrent requests cannot mint duplicate tokens.
- A request, including gate wait and retries, has a 45-second maximum. Each HTTP
  attempt is capped at 10 seconds. Redirects are disabled. Response bodies are
  bounded by the existing 1 MiB JSON decoder and never included in errors.
- Transport errors and HTTP 500/502/503/504 receive at most two retries with
  exponential jitter. Mutative calls are spaced at least one second apart.
  401/404/422 and ordinary permission-denied 403 are not retried automatically.
- Rate-limited 403/429 and positive `Retry-After` return a safe retryable error
  instead of spinning. `CredentialError.RetryAt` and an in-memory cooldown stop
  this client from requesting GitHub again before the backoff expires. The
  scheduler must respect that time across replicas/restarts; process memory is
  not a distributed rate limiter. [GitHub REST best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)
- Errors expose only fixed codes, status, retryability and backoff time. Do not
  install request/response logging transports that log Authorization or bodies.
  `InstallationToken` redacts formatting/JSON; `Value()` is an explicitly secret
  accessor, reserved for trusted checkout. Calling it and logging the returned
  string defeats that protection.

## IAM contract (deployment belongs to Task 40)

Do not broaden the existing API role. The dedicated trusted credential role
must contain this secret-specific statement (substitute the configured ARN):

```json
{
  "Effect": "Allow",
  "Action": "secretsmanager:GetSecretValue",
  "Resource": "<HAKO_GITHUB_APP_PRIVATE_KEY_SECRET_ARN>"
}
```

No `ListSecrets`, `PutSecretValue`, wildcard secret resource, or AWS access-key
management is needed. `VersionStage` is explicitly `AWSCURRENT`. A
customer-managed KMS key may additionally need narrowly scoped `kms:Decrypt`
with Secrets Manager service/encryption-context conditions; do not grant
general decrypt. Scope the CP DB role to required read-only registry tables and
use the existing transaction interface. No IAM role deployment, secret creation,
GitHub App registration, Terraform apply, or real-secret retrieval occurs in
Task 37.

## Rotation and emergency revocation

Routine rotation:

1. An authorized operator generates a new GitHub App key and stores its raw PEM
   as the secret's new `AWSCURRENT` version, without echoing it in tools/logs.
2. Verify with a controlled trusted-service request that the new key signs a
   valid App JWT and mints a read-only repository-scoped token. Local tests use
   generated keys and a fake GitHub server, not the real secret.
3. Remove the old key from GitHub only after all service instances see the new
   version. `ClearCache` or restarting instances forces token refresh, but
   routine key rotation does not itself revoke existing Installation tokens.

Emergency procedure:

1. Disable Run creation and token issuance; stop/terminate affected attempts.
2. Mark affected Installation bindings unavailable and suspend/uninstall or
   revoke access in GitHub. Remove a compromised App key from GitHub and rotate
   `AWSCURRENT`. These are explicit operator actions, not automated by this library.
3. Revoke known issued tokens through GitHub's authenticated token-revocation
   endpoint where possible; do not assume deleting an App key revokes all tokens.
   Clear/restart **all** credential-service replicas. `ClearCache` only drops
   local references, not remotely issued credentials.
4. Audit safe Tenant/Run/Installation IDs, CloudTrail secret reads and GitHub
   events without searching/displaying raw token/key values. Re-enable only
   after authorization, checkout and cleanup are verified.

## Verification

Unit tests use generated RSA keys and fake GitHub HTTP endpoints. They cover
claims/signature, scope, cache refresh/concurrency, suspension/revocation,
authorization changes, errors/redaction, rotation and bounded retry/cancellation.
PostgreSQL integration tests use a disposable schema on the always-running local
database and verify active binding, Tenant isolation and unavailable Installation
denial. Neither suite calls public GitHub or Secrets Manager.
