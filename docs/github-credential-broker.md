# Internal GitHub Credential Broker

Status: Local executable and HTTP composition implemented; production certificate lifecycle, IAM provisioning, private networking, and deployment remain outstanding. The Tenant App feature remains default-off.

## Boundary

`cmd/hako-github-broker` is the trusted boundary for Tenant-owned GitHub OAuth credentials and App private keys. The public API, CLI, queue messages, Resource Plane and Job VM do not receive AWS credentials or App private keys. The broker resolves the authoritative Tenant/App/Run identity from PostgreSQL/Aurora DSQL and reads only the purpose-specific approved Secret version through its dedicated AWS role.

```text
Control Plane checkout worker/API
        │ TLS 1.3 + mTLS, SPIFFE URI SAN
        ▼
Hako GitHub Credential Broker
        ├── DSQL/PostgreSQL: canonical setup state, Run, App binding
        ├── OAuth-purpose reader role → OAuth client secret
        └── private-key-purpose reader role → GitHub App private key
        │
        └── short-lived, repository-scoped Installation token
```

The broker's TLS listener requires a trusted client certificate. The HTTP handler then requires the exact client URI SAN `spiffe://hako/control-plane` before it invokes an OAuth resolver or checkout service. Use a private service endpoint; this listener is not a public API or SSH/data-plane gateway.

## Endpoints

| Endpoint | Input | Output / boundary |
|---|---|---|
| `POST /v1/oauth/setup-url` | Pinned Tenant/App setup identity and opaque state | Setup URL only; does not read a Secret |
| `POST /v1/oauth/authorize-url` | Pinned identity, state, PKCE verifier | Authorization URL only; does not read a Secret |
| `POST /v1/oauth/verify` | Pinned identity, OAuth code, expected Installation, PKCE verifier | Verified App/Installation identity only; OAuth secret stays in broker |
| `POST /v1/checkout/token` | Tenant ID and persisted Workload Run ID only | Short-lived token and expiry over mTLS with `Cache-Control: no-store` |

Checkout callers cannot choose App ID, Installation, Repository, Role ARN, Secret ARN, version, revision, generation, or commit. The broker loads the Workload Run and active exact credential pin from the database, mints a read-only repository-scoped token, then rechecks caller authorization, Run state, and the credential pin after GitHub I/O. Cancellation, disable, or rotation during the request fails closed. A failed Tenant-owned request never falls back to the shared App. Shared Runs continue using the existing shared credential provider.

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
| `HAKO_GITHUB_BROKER_LISTEN_ADDR` | Listener address; defaults to `:8443` |

TLS certificate, private key, and client CA must be configured together. TLS is at least 1.3, and the server requires a verified client certificate. Broker clients must present the Control Plane URI SAN, use the broker trust root, and refuse redirects. The checkout client has a bounded 50-second HTTP timeout; the OAuth client has a shorter timeout. Server read/header/write/idle limits are bounded.

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
