# Task 46: Hako Session Tickets and Gateway

Status: In Progress (Ed25519 Gateway-audience ticket claims, RBAC-backed session API, HTTPS/HMAC active-session lookup, TLS/WSS core, shell/port local E2E, fixed-port VS Code bridge, idle-lease reporting, structured connection logs, CloudWatch EMF metrics, an OCC-fenced shared cap of ten active connection leases per Workspace, cross-replica same-Session connection exclusion, and a shared limit of 30 new lease admissions per Workspace per minute are implemented. `cmd/hako-workspace-gateway` composes the verifier, HTTPS session/lease clients, S3 runtime journal, Lambda MicroVM SDK/token provider, Agent ticket signer, and journal-backed shell/VS Code directory into a non-root HTTPS service. A default-off Resource Plane Terraform contract now creates the 443-only ALB with a 60-minute WebSocket idle timeout, private ARM64 Fargate tasks, ephemeral Secret loader volume, exact journal/MicroVM/Secret IAM, CPU scaling, and health/5xx alarms; OpenTofu mock-plan tests cover disabled and enabled configurations. Actual AWS deployment, production key/secret/certificate/DNS/network provisioning, idle-sweep-to-Dispatcher runtime E2E, and live MicroVM connectivity remain. Priority: P1.

## Goal

Connect CLI/Browser to the correct Workspace through a zero-trust HTTPS/WSS proxy, without putting application data through the Control Plane.

## Work

- Issue short-lived, audience-bound Session Tickets only after Tenant/Workspace RBAC; bind user, Tenant, Workspace, Resource Plane, allowed channels, expiry, and unique ticket ID.
- Implement signature/key rotation, replay policy, expiry, revocation/authorization recheck, and audit-safe validation errors.
- Deploy a long-running ECS/Fargate Gateway with TLS/WSS, stream routing, rate/connection limits, and health/metrics.
- Keep TCP/22 and application/database ports private; Gateway is not an SSH bastion and must not trust caller-supplied routing IDs.

## Local implementation progress (2026-10-12)

- `internal/workspacesession` issues and verifies Ed25519 tickets with a maximum five-minute TTL, key IDs for public-key rotation, Gateway audience, random 192-bit ticket ID, User/Tenant/Workspace/Resource Plane binding, and the fixed `shell`, `vscode`, and `port-forward` permission vocabulary.
- `POST /v1/tenants/{tenant}/workspaces/{workspace}/sessions` checks membership and visibility in the same transaction as the session insertion, resolves Placement from the database, requires desired and observed `running`, stores the ticket ID/permissions, and audits without recording the ticket value. `DELETE .../sessions/{session_id}` revokes the record and audits atomically. Routes are disabled unless an issuer is injected.
- The API client has create/revoke methods. Local ticket unit tests and PostgreSQL API integration cover cross-Tenant and non-owner denial, state gating, route/Placement binding, invalid permissions, persistence, and revocation.
- `workspacesession.Verifier` performs fail-closed signature/audience/expiry/channel checks and requires an injected `ActiveSessionLookup` before connection upgrade. Lookup failures return a fixed error without leaking backend details; caller-supplied routing IDs are not part of this API. Unit tests cover revoked/inactive records, permission mismatch before lookup, canceled request context, and backend error redaction.
- `HTTPActiveSessionLookup` calls the Control Plane over HTTPS using a Plane-specific HMAC key. Its internal API endpoint compares the complete claims identity and permissions against the persisted session row and returns only the active bit. The endpoint is opt-in via `HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS`; no real Secret was read or provisioned. PostgreSQL integration verifies active, revoked, expired, mismatched-user, and wrong-HMAC behavior. See [Workspace Session Ticket](../docs/workspace-session-ticket.md).
- `internal/workspacegateway` now handles TLS-only shell, VS Code, and port-forward WSS routes. It verifies ticket/active session before upgrade, matches the signed Workspace and local Resource Plane, rejects URL query parameters and untrusted browser origins, resolves port allowlists through an injected policy, and routes only to an injected private `StreamDialer`. In-process global/per-Workspace connection caps and bounded WebSocket frame/chunk sizes are enforced. The Control Plane is not in the data-stream path. See [Workspace HTTPS/WSS Gateway](../docs/workspace-gateway.md).
- `workspaceagent.GatewayDialer` binds the signed Workspace claim to its Workspace-local PTY manager and starts only the configured fixed shell. A TLS WSS test sends a command through the Gateway and checks its output from the real local PTY. Port-forwarding is allowed only to `127.0.0.1:{port}` after the injected `PortAuthorizer` approves it; a TLS WSS integration test round-trips bytes through an ephemeral loopback listener. No client-controlled host or Control Plane data path is used. The Lambda MicroVM adapter supports a separate one-use VS Code ticket to the Guest Agent's fixed `127.0.0.1:8081` bridge, verified by Guest, adapter, and CLI/Gateway local E2E. The Workspace Agent now starts pinned/checksummed code-server as the EFS identity and stops it with the Agent; an actual ARM64 image/MicroVM run remains unverified.
- Added `workspaceagent.WorkspaceRouter`, a concurrency-safe process-local selector from verified Workspace ID to a Workspace-bound stream dialer. Duplicate registration fails closed; unregister is compare-and-delete so a delayed runtime cleanup cannot remove a replacement. Unit tests cover routing, unknown/empty claims, duplicate binding and stale cleanup, and a TLS WSS E2E exercises Gateway → router → real PTY composition. The router is not yet attached to Resource Plane lifecycle events or Gateway production deployment, and it does not provide cross-process discovery. See [Workspace HTTPS/WSS Gateway](../docs/workspace-gateway.md).
- Gateway tests cover active-session verifier integration, shell/PTY relay through the multi-Workspace router, route spoofing, wrong channels/origins, port policy and loopback port relay, VS Code fixed-loopback relay, oversized frames, per-Workspace connection cap, concurrent same-ticket replay rejection/reconnect, plaintext rejection, invalid upgrade preflight, and upstream failure. Production signer/lookup-key provisioning and rotation, actual code-server ARM64 image build, metrics ingestion, and Gateway deployment remain. See [Workspace HTTPS/WSS Gateway](../docs/workspace-gateway.md) and [Workspace Session Ticket](../docs/workspace-session-ticket.md).

## Progress update (2026-10-17)

Added `workspaceagent.LambdaMicroVMGatewayDialer` for the
Resource Plane shell and fixed VS Code bridge paths. It validates an immutable MicroVM endpoint against
the bound VM ID/Region, rechecks verified Tenant/Workspace/Resource Plane and
channel permission claims, mints a fresh 2-minute port-8080-only Lambda auth token
and one-use guest Agent Ticket, then opens the matching fixed WSS Guest route with both
credentials. A per-stream directory resolves the Workspace's exact current
ProviderID from its Resource Plane journal and verifies `GetMicrovm` reports
that same VM as running before dialing; this removes the need for an in-process
lifecycle registration map. SDK request scope, ticket identity,
subprotocol/route construction, invalid endpoints, foreign claims, journal
identity/state, and provider mismatch have local tests. The Gateway executable
composition and local ARM64 image now exist, but deployment, AWS token/IAM
provisioning, code-server image startup, and a live MicroVM stream remain open;
guest port-forward is not implemented by this adapter. See
[`Workspace HTTPS/WSS Gateway`](../docs/workspace-gateway.md).

## Acceptance criteria

- Tests cover cross-Tenant/workspace rejection, expired/forged/replayed ticket, permission mismatch, routing spoof, rate limit, reconnect, and upstream failure.
- End-to-end stream tests cover shell/PTY and one forwarded localhost port without Control Plane data proxying.
- Terraform ingress is HTTPS/WSS 443 only with least-privilege Runtime connectivity; logs exclude ticket values and terminal contents unless explicitly audited policy allows.
- Document key rotation and incident revocation.

## Progress update (2026-10-17, Gateway service composition)

Added `cmd/hako-workspace-gateway`, a long-running TLS 1.2+ HTTPS/WSS process
that composes the signed Session verifier and active-session Control Plane
lookup with the Resource Plane S3 journal, Lambda MicroVM `GetMicrovm` and
port-8080 auth-token APIs, one-use Agent-ticket signer, and journal-backed
shell stream resolver. It uses the AWS default credential chain (ECS task role
in deployment); it has no environment-based AWS keys and does not query DSQL.
The service runs non-root in a minimal ARM64 image, bounds headers and
connection counts, supports graceful shutdown, and exposes only the core's
static `/healthz` probe unauthenticated. Public verification-key JSON and
private/HMAC key files are size/type checked; private files reject symlinks
and group/world writable modes. Key/configuration tests use synthetic values.
`make build-workspace-gateway-image` builds the local Podman image.

This is local composition/build support, not deployment proof. Resource Plane
Terraform still does not create the Gateway service, listener/target group,
certificate handling, autoscaling, or the narrowly scoped task policy. No real
AWS Secret, AWS API, or MicroVM was accessed. The VS Code fixed-port guest
bridge is now composed and the image has a pinned code-server startup path; guest port
forwarding remains disabled pending trusted port-policy wiring. Key rotation/reload, cross-replica
connection/revocation controls, structured audit/metrics, and live E2E remain
acceptance gates.

## Progress update (2026-10-17, active Session revocation)

The Gateway now revalidates the full signed Session identity through the
existing HTTPS/HMAC Control Plane lookup every 15 seconds on an established
stream (configurable up to one minute). A revoked/expired Session or lookup
failure cancels both proxy directions and closes the client/runtime streams;
application bytes remain Gateway-to-Resource-Plane and never transit the
Control Plane. WSS tests exercise revocation and fail-closed lookup failure.
This is per-process and is not a substitute for distributed connection limits
or production runtime validation.

## Progress update (2026-10-24, Gateway telemetry)

The Gateway now emits one structured `workspace_gateway.connection` log for
each stream attempt, with fixed channel/outcome fields, duration, current
process connection count, and authenticated Tenant/Workspace identity when
available. It excludes Session ticket values and stream contents. When the
service entrypoint is used, it also writes CloudWatch Embedded Metric Format
records for connection events, rejected connections, active connections, and
duration. Metric dimensions are limited to Resource Plane, fixed channel, and
fixed outcome; Tenant, Workspace, ticket, and error text are not dimensions.
The focused Gateway and service-entrypoint Go tests pass. These metrics are
process-local snapshots and do not implement fleet-wide connection/rate
coordination or prove production CloudWatch ingestion.

## Progress update (2026-10-24, shared connection admission)

The Control Plane now enforces a maximum of ten active connection leases per
Workspace allocation. The lease transaction reads the current
`workspace_status` row `FOR UPDATE`, counts unexpired, unreleased leases for
that exact MicroVM/allocation generation, and inserts only below the cap.
Aurora DSQL evaluates `FOR UPDATE` through OCC conflict adjudication rather
than blocking row locks, so concurrent Gateway replicas conflict on this
shared row and the existing transaction retry reruns the count before insert.
An existing lease can renew at capacity; expired/released leases do not count.
The API returns HTTP 429 when admission is full, and the Gateway preserves
that response through its Lambda MicroVM stream directory. PostgreSQL tests
race twenty independent lease admissions, verify exactly ten succeed, check
renewal at capacity, and verify a slot is available after release.

The lease API also requires the Session Ticket ID and atomically claims the
matching, unexpired `sessions` row for one active Gateway lease. The ticket
value is not stored. A second lease using the same Session gets HTTP 409;
closing the owning lease clears only its own claim, and an expired claim can
be recovered after a Gateway failure. The claim is bounded by both the lease
and Session expiry. Tests cover the concurrent two-replica race, release and
reconnect, and HMAC-authenticated API behavior against PostgreSQL. The
The shared fixed-window admission counter allows 30 new connection leases per
Workspace per minute across Gateway replicas. It persists even rejected
capacity/rate attempts, saturates at 31, resets after one minute, and does not
count an existing lease heartbeat. PostgreSQL API tests verify the threshold,
HTTP 429 response, and committed counter; concurrent lease tests continue to
verify shared OCC admission. Production deployment, key provisioning, and
live MicroVM connectivity remain open.
