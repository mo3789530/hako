# Internal GitHub Credential Broker

Status: Local executable and HTTP composition implemented; production certificate lifecycle, IAM provisioning, private networking, and deployment remain outstanding. The Tenant App feature remains default-off.

## Boundary

`cmd/hako-github-broker` is the trusted boundary for Tenant-owned GitHub OAuth credentials and App private keys. The public API, CLI, queue messages, Resource Plane and Job VM do not receive AWS credentials or App private keys. The broker resolves the authoritative Tenant/App/Run identity from PostgreSQL/Aurora DSQL and reads only the purpose-specific approved Secret version through its dedicated AWS role.

```text
Control Plane API / GitHub Checks worker
        │ TLS 1.3 + mTLS, SPIFFE URI SAN
        ▼
Hako GitHub Credential Broker
        ├── DSQL/PostgreSQL: canonical setup state, Run, App binding
        ├── OAuth-purpose reader role → OAuth client secret
        └── private-key-purpose reader role → GitHub App private key
        │
        └── short-lived, repository-scoped Installation token
```

The broker's TLS listener requires a trusted client certificate. OAuth setup and GitHub Checks endpoints require the exact `spiffe://hako/control-plane` URI SAN. The checkout endpoint accepts that identity plus only Resource Plane identities explicitly listed in `HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS` (comma-separated stable Plane IDs); it does not grant those callers OAuth or Checks access. The ephemeral Actions runner cleanup endpoint accepts a separate exact Plane allowlist in `HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS`. It receives only the durable integration/job/attempt/claim key, then verifies the unexpired cleanup lease and Plane pin in the Control Plane database before resolving the Tenant App credential and removing that exact repository runner. Tenant, Installation, Repository, and Runner IDs are never accepted from the Resource Plane request. The Resource Plane worker uses `spiffe://hako/resource-plane/<resource-plane-id>` and a client constructor that checks the same exact identity. Use a private service endpoint; this listener is not a public API or SSH/data-plane gateway.

## Endpoints

| Endpoint | Input | Output / boundary |
|---|---|---|
| `POST /v1/oauth/setup-url` | Pinned Tenant/App setup identity and opaque state | Setup URL only; does not read a Secret |
| `POST /v1/oauth/authorize-url` | Pinned identity, state, PKCE verifier | Authorization URL only; does not read a Secret |
| `POST /v1/oauth/verify` | Pinned identity, OAuth code, expected Installation, PKCE verifier | Verified App/Installation identity only; OAuth secret stays in broker |
| `POST /v1/checkout/token` | Tenant ID and persisted Workload Run ID only | Short-lived token and expiry over mTLS with `Cache-Control: no-store` |
| `POST /v1/actions-runner/registration-token` | Integration ID, workflow job ID, run attempt, and current claim ID only | One repository-scoped registration token over Control Plane-only mTLS with `Cache-Control: no-store` |
| `POST /v1/actions-runner/launch` | Integration ID, workflow job ID, run attempt, and current claim ID only | Consumes the live claim and pins the caller's allowlisted Resource Plane; returns bounded repository/job metadata, never a runner token |
| `POST /v1/actions-runner/bind` | Integration ID, workflow job ID, run attempt, claim ID, and Lambda MicroVM ID | Binds the launched VM to the exact live claim; Plane ID is derived from the verified certificate |
| `POST /v1/actions-runner/bootstrap-token` | Integration ID, workflow job ID, run attempt, claim ID, and bound Lambda MicroVM ID | One-time opaque registration token grant, only after the exact Plane+VM binding is verified |
| `POST /v1/actions-runner/startup-failed` | Integration ID, workflow job ID, run attempt, claim ID, and bounded failure category | Marks the exact live claim failed and creates its identity-fenced cleanup outbox record |
| `POST /v1/actions-runner/deregister` | Integration ID, workflow job ID, run attempt, and current cleanup claim ID only | Empty acknowledgement; Plane-specific mTLS and the live Control Plane cleanup lease authorize the exact repository runner removal |

Checkout callers cannot choose App ID, Installation, Repository, Role ARN, Secret ARN, version, revision, generation, or commit. The broker loads the Workload Run and active exact credential pin from the database, mints a read-only repository-scoped token, then rechecks caller authorization, Run state, and the credential pin after GitHub I/O. Cancellation, disable, or rotation during the request fails closed. A failed Tenant-owned request never falls back to the shared App. Shared Runs continue using the existing shared credential provider.

The Resource Plane startup endpoints (`launch`, `bind`, `bootstrap-token`, and
`startup-failed`) accept only the exact configured Resource Plane SPIFFE
identity. Plane identity is derived from the client certificate rather than
request JSON. The launch claim is idempotently replayable only by the same
Plane, while live, and before token issuance; token issuance requires the
persisted MicroVM binding and is consumed at most once per attempt. The Lambda
runtime loads its mTLS certificate/key, broker CA and signing key from four
separate Secret ARNs using its execution role. The local composition is fake-
tested and is not deployed; its queue publisher/event-source mapping, exact
IAM, guest listener/image and Check reporter remain separate work.

The trusted `githubapp.TokenClient` also has a repository-scoped Actions runner
registration-token primitive for Tenant-owned Apps. It requires the exact
active Tenant App revision/generation and the independent per-repository
`actions_runner_enabled` policy, requests only the `administration:write`
Installation permission for that one Repository, and rechecks authorization
after GitHub returns. Neither the temporary Installation token nor the
one-hour runner registration token is cached; both token types redact common
formatting and JSON paths. The mTLS endpoint is wired to the durable claim
resolver: it atomically consumes a live claim by transitioning `claimed` to
`starting`, resolves the Tenant/App/Repository from the database, then calls
the token primitive. A second request cannot mint for the consumed claim. Mint
failures mark the start failed; a controller crash leaves a `starting` row
non-reclaimable until the token safety window (65 minutes after the original
lease expiry) passes.
Installations must grant their Tenant GitHub App `Administration: write` for
repository runners; installations without it fail closed.

The checkout token is passed only to the trusted fixed-SHA checkout implementation, which supplies it to Git through a private askpass pipe. It is not persisted in the Run, database, queue, artifacts, or logs. The broker response is short-lived transport data, not a credential distribution API for Job VMs.

## Local configuration contract

The executable accepts these environment variables:

| Variable | Requirement |
|---|---|
| `HAKO_DATABASE_URL` | Local PostgreSQL connection URL; mutually exclusive with `HAKO_DSQL_HOST` |
| `HAKO_DSQL_HOST` | Aurora DSQL endpoint when not using local PostgreSQL |
| `HAKO_DSQL_USER` | DSQL IAM database user; defaults to `admin` |
| `HAKO_DSQL_DATABASE` | DSQL database name |
| `AWS_REGION` | AWS SDK region for DSQL/STS/Secrets Manager |
| `HAKO_GITHUB_BROKER_CALLBACK_URL` | Exact OAuth callback URL configured in the App |
| `HAKO_GITHUB_BROKER_TLS_CERT_FILE` | Broker HTTPS server certificate PEM |
| `HAKO_GITHUB_BROKER_TLS_KEY_FILE` | Matching server key PEM |
| `HAKO_GITHUB_BROKER_CLIENT_CA_FILE` | CA bundle used to verify Control Plane client certificates |
| `HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to call only `/v1/checkout/token`; defaults to none |
| `HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to call only `/v1/actions-runner/deregister`; defaults to none |
| `HAKO_GITHUB_BROKER_LISTEN_ADDR` | Listener address; defaults to `:8443` |

TLS certificate, private key, and client CA must be configured together. TLS is at least 1.3, and the server requires a verified client certificate. API/OAuth/Checks clients must present the Control Plane URI SAN. Checkout clients may use the Control Plane URI or an explicitly allowlisted Resource Plane URI, use the broker trust root, and refuse redirects. The checkout client has a bounded 50-second HTTP timeout; the OAuth client has a shorter timeout. Server read/header/write/idle limits are bounded.

Local run example (uses the development PostgreSQL service and does not apply migrations or contact GitHub/AWS unless the respective operations are invoked):

```sh
HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable' \
HAKO_GITHUB_BROKER_CALLBACK_URL='https://hako.example/github/callback' \
HAKO_GITHUB_BROKER_TLS_CERT_FILE=/run/hako/broker-cert.pem \
HAKO_GITHUB_BROKER_TLS_KEY_FILE=/run/hako/broker-key.pem \
HAKO_GITHUB_BROKER_CLIENT_CA_FILE=/run/hako/control-plane-ca.pem \
go run ./cmd/hako-github-broker
```

Do not use a production key or real Tenant Secret for local tests. Unit and integration tests use fake token/Secret providers; PostgreSQL integration tests create isolated schemas and remove only those schemas.

## Production work not performed

- Provision the broker workload role with only the required DSQL access, exact purpose-specific `sts:AssumeRole` permissions, CloudWatch logging/metrics, and no MicroVM/Workspace lifecycle permissions.
- Issue and rotate broker server/client certificates from a dedicated trust root; monitor expiry and reject stale/revoked identities. The repository does not provision or distribute these certificates.
- Put the service on a private authenticated network path and configure the API/worker's broker URL, client identity and CA bundle.
- Deploy and test the API/worker integration, exact Role trust policies, Secret versions, and recovery/alerting. Do not enable Tenant-owned App activation until Tasks 60 and 71 pass.
- AWS `apply`, live GitHub changes, real Secret retrieval and Tenant cutover require explicit approval.
