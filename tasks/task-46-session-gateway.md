# Task 46: Hako Session Tickets and Gateway

Status: In Progress (Ed25519 Gateway-audience ticket claims, channel permissions, RBAC-backed create/revoke API routes, an HTTPS/HMAC active-session API/client with PostgreSQL identity matching, and a TLS-only WSS Gateway core with signed-Workspace-bound PTY/loopback-port adapters and local shell/port E2E now exist. A local in-process multi-Workspace router selects only from verified Workspace claims, rejects duplicate registration, and fences delayed unregister against a replacement. A ticket allows one concurrent stream per process and can reconnect after close; cross-replica coordination is not implemented. Production signer/lookup-key provisioning and Gateway configuration, runtime lifecycle registration, VS Code adapter, fleet-wide revocation/rate limits, metrics, deployment, and MicroVM connectivity remain). Priority: P1.

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
- `workspaceagent.GatewayDialer` binds the signed Workspace claim to its Workspace-local PTY manager and starts only the configured fixed shell. A TLS WSS test sends a command through the Gateway and checks its output from the real local PTY. Port-forwarding is allowed only to `127.0.0.1:{port}` after the injected `PortAuthorizer` approves it; a TLS WSS integration test round-trips bytes through an ephemeral loopback listener. No client-controlled host or Control Plane data path is used. VS Code remains unavailable until its runtime adapter exists.
- Added `workspaceagent.WorkspaceRouter`, a concurrency-safe process-local selector from verified Workspace ID to a Workspace-bound stream dialer. Duplicate registration fails closed; unregister is compare-and-delete so a delayed runtime cleanup cannot remove a replacement. Unit tests cover routing, unknown/empty claims, duplicate binding and stale cleanup. The router is not yet attached to Resource Plane lifecycle events or Gateway production composition, and it does not provide cross-process discovery. See [Workspace HTTPS/WSS Gateway](../docs/workspace-gateway.md).
- Gateway tests cover active-session verifier integration, shell/PTY relay, route spoofing, wrong channels/origins, port policy and loopback port relay, oversized frames, per-Workspace connection cap, concurrent same-ticket replay rejection/reconnect, plaintext rejection, invalid upgrade preflight, and upstream failure. Production signer/lookup-key provisioning and Gateway configuration, multi-Workspace Resource Plane router, VS Code runtime adapter, distributed connection/rate controls, metrics/audit, deployment and active-stream revocation remain. See [Workspace HTTPS/WSS Gateway](../docs/workspace-gateway.md) and [Workspace Session Ticket](../docs/workspace-session-ticket.md).

## Acceptance criteria

- Tests cover cross-Tenant/workspace rejection, expired/forged/replayed ticket, permission mismatch, routing spoof, rate limit, reconnect, and upstream failure.
- End-to-end stream tests cover shell/PTY and one forwarded localhost port without Control Plane data proxying.
- Terraform ingress is HTTPS/WSS 443 only with least-privilege Runtime connectivity; logs exclude ticket values and terminal contents unless explicitly audited policy allows.
- Document key rotation and incident revocation.
