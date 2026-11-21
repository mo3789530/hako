# Workspace HTTPS/WSS Gateway

`internal/workspacegateway` is the local Gateway HTTP core for interactive
Workspace connections. It requires a TLS request, validates a bearer Hako
Session Ticket through `workspacesession.Verifier` before upgrading, and only
routes from signed claims. The route's Workspace ID must match the ticket and
the ticket Resource Plane must match this Gateway instance. Query strings are
rejected so tickets or client routing hints cannot be smuggled through URLs.

Routes are:

- `GET /v1/workspaces/{workspace_id}/shell` (`shell` permission)
- `GET /v1/workspaces/{workspace_id}/ssh` (`ssh-proxy` permission; fixed loopback TCP port 22)
- `GET /v1/workspaces/{workspace_id}/vscode` (`vscode` permission)
- `GET /v1/workspaces/{workspace_id}/ports/{port}` (`port-forward` permission)
- `GET /v1/workspaces/{workspace_id}/preview-logs` (`preview-logs` permission; fixed, bounded, read-only Guest operation)
- `GET /healthz` (unauthenticated health probe)

All stream routes negotiate `hako.workspace.v1`. Browser Origins must exactly
match configured HTTPS/HTTP origins; native CLI connections may omit Origin.
Port forwarding requires an injected `PortAuthorizer` to resolve the allowed
port from trusted Workspace state. The client cannot supply a host or arbitrary
destination. The production Gateway first permits the fixed Preview application
port for a generation-specific Preview identity; ordinary Workspaces use a
signed HTTPS/HMAC authorization request to the Control Plane. That endpoint
checks the persisted Session claims, running Workspace state, current Lambda
MicroVM allocation, and a healthy Service's catalog-declared TCP port. Missing,
stale, unhealthy, or undeclared Services are denied. Only the authorization
metadata reaches the Control Plane; the Resource Plane `StreamDialer` opens the
private runtime connection and carries the data stream without proxying bytes
through the Control Plane or querying Aurora DSQL from the Gateway.
`workspaceagent.GatewayDialer`
is a local adapter for one Workspace's fixed-shell PTY manager and private
ports: it checks the signed Workspace ID, opens `/bin/sh` in that Workspace,
and dials only `127.0.0.1:{port}` after Gateway `PortAuthorizer` approval. The
SSH route has a separate signed permission and always dials `127.0.0.1:22`; it
does not use the caller-controlled port route or public SSH ingress. The
local `GatewayDialer` still rejects VS Code; the Lambda MicroVM adapter below
provides a distinct fixed VS Code bridge. The Resource Plane
must map signed Workspace claims to the right runtime; caller-provided
hostnames are never accepted. Static local registrations are provided by
`workspaceagent.WorkspaceRouter`; Lambda MicroVM runtimes can instead use the
durable-journal-backed `WorkspaceLambdaMicroVMStreamDirectory` described
below. Active session/revocation state
is checked through the verifier's injected `ActiveSessionLookup`; the local
`HTTPActiveSessionLookup` uses the signed HTTPS/HMAC Control Plane endpoint,
which checks the complete claims identity against PostgreSQL and returns only
the active bit. The Gateway repeats this metadata-only authorization check
every 15 seconds while a WebSocket is open (bounded to at most once per
minute); revocation, ticket expiry, or lookup failure closes both stream
directions. Workspace bytes still flow directly between the client and
Resource Plane runtime. `workspaceagent.WorkspaceRouter` is a local in-process router
that selects the registered single-Workspace dialer using the verified
Workspace claim. It rejects duplicate live registrations, and cleanup must
compare against the expected dialer so a delayed removal cannot unregister a
replacement. Unknown Workspace IDs fail closed. Registration is an internal
Resource Plane responsibility; it does not accept host/port destinations or
discover runtimes. It is intended for local/runtime-bound adapters, not as the
Lambda MicroVM fleet directory.
No terminal/application payload is sent to the Control Plane.

For Lambda MicroVM-backed Workspaces, `workspaceagent.LambdaMicroVMGatewayDialer`
implements the Resource Plane's guest shell path. Its immutable binding
contains Tenant/Workspace/Resource Plane IDs, MicroVM ID, Region, and the
provider endpoint; constructor validation requires the exact AWS MicroVM host
for that ID and Region. After Gateway Session authorization it issues a fresh
two-minute Agent Ticket and asks the Lambda MicroVM SDK for a two-minute AWS
auth token scoped only to TCP port 8080. It then opens the fixed WSS Guest shell
route with the Hako ticket in `Authorization`, and AWS auth/port values in the
Lambda-required `lambda-microvms.authentication.*` and `lambda-microvms.port.8080`
WebSocket subprotocols (which Lambda strips before forwarding). Neither
credential is persisted or logged. The token provider/dialer and
authorization binding are locally tested, and `cmd/hako-workspace-gateway`
composes them with mounted keys and the journal-backed resolver; a real Lambda endpoint connection remains unverified. This adapter supports shell, the
fixed VS Code bridge, and authorized TCP port-forward channels. Port-forward
requests use a one-use Agent Ticket bound to the exact Workspace, MicroVM
allocation, permission, and requested port; the Guest Agent dials only
`127.0.0.1:{port}`. The Gateway policy limits ordinary Workspaces to ports
declared by a healthy Service in the current runtime generation. Preview uses
its fixed application port. The Workspace image must start code-server on
`127.0.0.1:8081` and configure its own authentication. The directory reads the matching Workspace
journal from the Resource Plane store and calls `GetMicrovm` for each new
connection, requiring exact Tenant/Workspace/Plane identity, a non-tombstoned
`running` journal, and the same running provider ID before creating the
ephemeral dialer. It resolves lifecycle state without process-local
registration or client routing hints. The Gateway process wires this resolver
to the S3 store and AWS SDK; least-privilege deployment IAM and live MicroVM
connectivity remain unverified.

The Gateway bounds concurrent total and per-Workspace connections in-process,
and applies a maximum payload size to every incoming WebSocket frame and every
outgoing chunk (64 KiB default, 1 MiB maximum). Separately, the signed lease API
admits at most ten unexpired connections for the current Workspace allocation
across Gateway replicas. It returns HTTP 429 at capacity; the Gateway preserves
that status for clients. The cap is enforced transactionally against shared
Control Plane metadata, not application data. The same lease API also claims
the persisted Session row for one active connection, so concurrent reuse of
one signed Session Ticket across Gateway replicas receives HTTP 409. Release
clears the claim only for its owning lease; claim expiry permits recovery
after Gateway failure. Only the opaque Session ID and active lease metadata
are stored; the signed ticket value is not. Stream contents and tickets are
not logged. `/healthz` returns only static readiness status. Per-process caps
remain local. A shared Workspace status counter also limits new lease
admissions to 30 per minute across Gateway replicas; heartbeats for existing
leases are exempt, and HTTP 429 identifies either capacity or start-rate
rejection. This is a connection-admission guard, not an application-traffic
rate limit. TLS termination, private
Gateway-to-runtime transport, and task/service health wiring are deployment
responsibilities.

Local WSS tests exercise shell byte relay and a Gateway → WorkspaceRouter →
Workspace-bound PTY command path, dedicated SSH-proxy permission and fixed `127.0.0.1:22` routing, a
port-forward byte round-trip to an ephemeral `127.0.0.1` listener,
wrong channel/Workspace/Origin, plaintext rejection, denied/allowed port
forwarding, per-Workspace connection cap, concurrent ticket replay rejection
with reconnect-after-close, active-stream revocation and fail-closed lookup
failure, upstream failure, and oversized frames. The concrete authenticated
Gateway composition exists in the local service entrypoint. Each stream
attempt emits a structured `workspace_gateway.connection` log. The service
entrypoint also emits CloudWatch Embedded Metric Format records for connection
events, rejected connections, active process connections, and duration. Metric
dimensions are limited to Resource Plane, fixed channel, and fixed outcome;
Tenant/Workspace IDs, tickets, and stream contents are excluded. These are
process-local metrics. The signed lease API enforces the shared ten-connection
per-Workspace allocation cap and rejects concurrent reuse of the same Session
Ticket across replicas with HTTP 409, and applies a shared fixed-window limit
of 30 new lease admissions per Workspace per minute. The VS Code process bridge
and Lambda MicroVM end-to-end stream PoC also remain open Task 46/47 work.
If an active Session recheck fails, the stream closes and the connection metric
uses the bounded `session_recheck_failed` outcome instead of grouping it with a
normal `disconnected` result. The metric still excludes Tenant, Workspace,
ticket, and raw error details.

## Resource Plane Gateway process

`cmd/hako-workspace-gateway` is the long-running HTTPS/WSS process entrypoint.
It composes the session verifier, signed HTTPS/HMAC active-session and
connection-lease clients, S3-backed Workspace runtime journal, Lambda MicroVM
lookup/auth-token client, and one-use Agent-ticket signer. Before opening the
Guest WSS stream it creates a lease bound to Tenant, Workspace, Resource Plane,
MicroVM, and allocation generation. It renews that lease every 30 seconds with
a 90-second expiry and releases it on stream close; missed release is recovered
by expiry. A heartbeat failure closes the stream so a Workspace cannot remain
connected without a durable lease. It resolves a fresh runtime allocation per
connection and never reads Aurora DSQL or accepts AWS credentials from
environment variables. AWS SDK default credentials are expected to come from
the Resource Plane ECS task role. The included
`cmd/hako-workspace-gateway/Dockerfile` builds a non-root ARM64 image, and
`make build-workspace-gateway-image` builds it locally with Podman.

The process requires these deployment inputs:

| Setting | Purpose |
| --- | --- |
| `HAKO_RESOURCE_PLANE_ID`, `AWS_REGION` | Bind this Gateway to one Resource Plane and AWS region |
| `HAKO_GATEWAY_LISTEN_ADDRESS` | HTTPS bind address; defaults to `:8443` |
| `HAKO_GATEWAY_TLS_CERT_FILE`, `HAKO_GATEWAY_TLS_KEY_FILE` | Mounted TLS certificate and private key |
| `HAKO_GATEWAY_PUBLIC_ORIGIN` | Fixed HTTPS origin sent to the Workspace Agent |
| `HAKO_GATEWAY_ALLOWED_ORIGINS` | Comma-separated exact browser origins |
| `HAKO_GATEWAY_SESSION_RECHECK_INTERVAL` | Active-session revocation/expiry check interval (`1s`–`1m`, default `15s`) |
| `HAKO_GATEWAY_SESSION_PUBLIC_KEYS_FILE` | JSON map of signing key ID to raw-base64 Ed25519 public key; supports verification key overlap |
| `HAKO_GATEWAY_SESSION_LOOKUP_ENDPOINT` | HTTPS Control Plane origin for active-session revalidation |
| `HAKO_GATEWAY_SESSION_LOOKUP_KEY_FILE` | Raw per-Resource-Plane HMAC key bytes (at least 32 bytes), mounted as a secret; also signs port authorization requests |
| `HAKO_GATEWAY_CONNECTION_LEASE_ENDPOINT` | HTTPS Control Plane origin for signed Workspace connection lease renew/release |
| `HAKO_GATEWAY_CONNECTION_LEASE_KEY_FILE` | Raw per-Resource-Plane HMAC key bytes (at least 32 bytes), mounted as a secret |
| `HAKO_GATEWAY_AGENT_TICKET_KEY_ID`, `HAKO_GATEWAY_AGENT_TICKET_KEY_FILE` | Active signing key ID and mounted Ed25519 PKCS#8 PEM private key |
| `HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET`, optional `HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX` | Resource Plane journal location |
| `HAKO_GATEWAY_MAX_CONNECTIONS`, `HAKO_GATEWAY_MAX_CONNECTIONS_PER_WORKSPACE` | In-process caps; defaults to 500 and 10 |

Private/HMAC key paths must be regular non-symlink files and not group/world
writable. Secrets are not logged. A restart is currently required to rotate
mounted keys; coordinate Session signer verification overlap and Guest Agent
public-key image updates operationally. Resource Plane Terraform now provides
a default-off Fargate/ALB deployment contract with a 60-minute WebSocket idle
timeout, private task, HTTPS target group, exact MicroVM/journal/Secret IAM,
ephemeral Secret loader volume, CPU
scaling, and target-health/ALB-5xx alarms. It has only been checked with
OpenTofu mock plans; no AWS apply, Secret retrieval, DNS change, or live
MicroVM access occurred. The Secret loader and Gateway application share the
ECS task role because ECS assigns one task role to every container; that role
is restricted to the six exact Secret ARNs and configured runtime resources.
The process currently enables shell and the fixed VS Code bridge; guest port
forward fails closed. The VS Code bridge requires code-server to run at the
fixed guest-loopback port and does not change its authentication policy.
`/healthz` is an unauthenticated static liveness check,
not proof of downstream AWS or Control Plane reachability. See Task 46 for
remaining deployment and live-validation gates.

The Preview log route is distinct from general file transfer. A public Session
may carry only the `preview-logs` permission, and the Gateway internally opens
the Guest file channel with a new fixed Resource Plane ticket, requests only
the `preview_logs` operation, and exposes a read-only stream capped at 256
KiB. Ordinary Workspace Session creation cannot mint this permission. Log
bytes remain in the Guest's bounded memory buffer and are not persisted.

### Workload artifact download

The Gateway can optionally expose `GET /v1/workload-artifacts/{artifact_id}`
when both `HAKO_WORKLOAD_ARTIFACT_BUCKET` and
`HAKO_WORKLOAD_ARTIFACT_PREFIX` are configured. The route requires TLS and a
two-minute HMAC ticket for the exact artifact and Resource Plane. It asks the
Control Plane over the signed internal grant endpoint to revalidate the
Tenant/Run authorization, then streams the bounded S3 object directly to the
client. It rejects query strings and never accepts a caller-supplied storage
URI. The Gateway task role receives `s3:GetObject` only for the configured
prefix; leaving the optional bucket settings empty disables this route.
`hako workload artifact download <tenant> <run> <artifact> --out <path>`
verifies the size and SHA-256 before installing the local file. The route,
authorization, and exact-prefix policy are covered by local tests; no AWS
deployment or live S3 retrieval has been performed.
