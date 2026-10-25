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

The broker's TLS listener requires a trusted client certificate. OAuth setup, GitHub Checks, and Agent Issue-context endpoints require the exact `spiffe://hako/control-plane` URI SAN. The checkout endpoint accepts that identity plus only Resource Plane identities explicitly listed in `HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS` (comma-separated stable Plane IDs); it does not grant those callers OAuth, Checks, or Agent context access. The ephemeral Actions runner cleanup endpoint accepts a separate exact Plane allowlist in `HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS`. It receives only the durable integration/job/attempt/claim key, then verifies the unexpired cleanup lease and Plane pin in the Control Plane database before resolving the Tenant App credential and removing that exact repository runner. The Agent context endpoint accepts only an Integration ID and signed inbox Delivery ID. It resolves Tenant membership, installation/repository binding, Factory opt-in, and credential revision from the database; revalidates them after GitHub reads; and returns only a bounded open Issue plus immutable default-branch SHA. The request cannot specify Tenant, Installation, Repository, ref, or SHA. The separate Agent task endpoint accepts an exact Tenant/Run/attempt/generation/Plane tuple from a Resource Plane identity allowlisted in `HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS`. It returns task text only when that is the current unexpired `agent` attempt on that Plane, and the task's immutable commit, branch, App integration, Installation, and Repository still match the Run. The API role cannot read `workload_agent_tasks`; the broker role has read-only access. Responses are bounded and `no-store`. Tenant, Installation, Repository, and Runner IDs are never accepted as authority from Resource Plane requests. The Resource Plane worker uses `spiffe://hako/resource-plane/<resource-plane-id>` and a client constructor that checks the same exact identity. Use a private service endpoint; this listener is not a public API or SSH/data-plane gateway.

## Endpoints

| Endpoint | Input | Output / boundary |
|---|---|---|
| `POST /v1/oauth/setup-url` | Pinned Tenant/App setup identity and opaque state | Setup URL only; does not read a Secret |
| `POST /v1/oauth/authorize-url` | Pinned identity, state, PKCE verifier | Authorization URL only; does not read a Secret |
| `POST /v1/oauth/verify` | Pinned identity, OAuth code, expected Installation, PKCE verifier | Verified App/Installation identity only; OAuth secret stays in broker |
| `POST /v1/environment/approval/verify` | Tenant, Software Catalog service, Installation/Repository IDs, Environment | Boolean verification only; broker re-resolves active App/policy, uses a repository-scoped `actions:read` token, and checks `required_reviewers` |
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
| `HAKO_GITHUB_BROKER_TLS_CERT_PEM` | Optional server certificate PEM injected by a container secret provider |
| `HAKO_GITHUB_BROKER_TLS_KEY_PEM` | Matching server private-key PEM; use with the other PEM variables instead of file variables |
| `HAKO_GITHUB_BROKER_CLIENT_CA_PEM` | Client CA bundle PEM injected by a container secret provider |
| `HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to call only `/v1/checkout/token`; defaults to none |
| `HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to call only `/v1/actions-runner/deregister`; defaults to none |
| `HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to read active Agent task context via `/v1/agent/task`; defaults to none |
| `HAKO_GITHUB_BROKER_AGENT_TASK_RESOURCE_PLANE_IDS` | Optional comma-separated Resource Plane IDs permitted to read the matching active Agent Run task through `/v1/agent/task`; defaults to none |
| `HAKO_GITHUB_BROKER_LISTEN_ADDR` | Listener address; defaults to `:8443` |

The Agent Issue-context route is Control Plane-only and has no separate Plane allowlist. It is currently a read-only broker capability; it does not create or schedule an Agent Run. The inbox processor must still atomically revalidate the command and persist a delivery-idempotent Agent Run with this immutable SHA before execution is enabled.

The Environment approval route is also Control Plane-only. Its caller cannot
choose App, Secret, or repository names; the broker checks the exact Catalog
service and OIDC policy, resolves the active Tenant App and Installation from
DSQL, and re-resolves the same credential generation after the GitHub request.
The short-lived installation token grants only `actions:read` for the one
repository. The `hako_github_broker` DSQL role needs `SELECT` on
`tenant_software_catalog_services` for this lookup; it gains no Catalog write
permission. Configure the API's existing
`HAKO_TENANT_GITHUB_OAUTH_BROKER_*` mTLS settings to enable the verifier. With
those settings absent, enabled approval policies fail closed as unavailable.

TLS certificate, private key, and client CA must be configured together. TLS is at least 1.3, and the server requires a verified client certificate. API/OAuth/Checks clients must present the Control Plane URI SAN. Checkout clients may use the Control Plane URI or an explicitly allowlisted Resource Plane URI, use the broker trust root, and refuse redirects. The checkout client has a bounded 50-second HTTP timeout; the OAuth client has a shorter timeout. Server read/header/write/idle limits are bounded.

Use either all three `*_FILE` variables or all three `*_PEM` variables; mixed
or partial configuration fails closed. The PEM form supports ECS Secrets Manager
injection without putting secret contents in Terraform inputs or task
definitions. Pass only Secret ARNs through IaC, restrict task execution-role
reads to those exact ARNs, and do not log the values.

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

Build the non-root ARM64 container image locally with
`make build-github-broker-image`. This builds
`hako-github-broker:local`; publishing it to ECR and deploying a private ECS
service are separate release operations and are not performed by this target.

## Production work not performed

- Provision the broker workload role with only the required DSQL access, exact purpose-specific `sts:AssumeRole` permissions, CloudWatch logging/metrics, and no MicroVM/Workspace lifecycle permissions.
- Issue and rotate broker server/client certificates from a dedicated trust root; monitor expiry and reject stale/revoked identities. The repository does not provision or distribute these certificates.
- Put the service on a private authenticated network path and configure the API/worker's broker URL, client identity and CA bundle.
- Deploy and test the API/worker integration, exact Role trust policies, Secret versions, and recovery/alerting. Do not enable Tenant-owned App activation until Tasks 60 and 71 pass.
- AWS `apply`, live GitHub changes, real Secret retrieval and Tenant cutover require explicit approval.
