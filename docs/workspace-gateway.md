# Workspace HTTPS/WSS Gateway

`internal/workspacegateway` is the local Gateway HTTP core for interactive
Workspace connections. It requires a TLS request, validates a bearer Hako
Session Ticket through `workspacesession.Verifier` before upgrading, and only
routes from signed claims. The route's Workspace ID must match the ticket and
the ticket Resource Plane must match this Gateway instance. Query strings are
rejected so tickets or client routing hints cannot be smuggled through URLs.

Routes are:

- `GET /v1/workspaces/{workspace_id}/shell` (`shell` permission)
- `GET /v1/workspaces/{workspace_id}/vscode` (`vscode` permission)
- `GET /v1/workspaces/{workspace_id}/ports/{port}` (`port-forward` permission)
- `GET /healthz` (unauthenticated health probe)

All stream routes negotiate `hako.workspace.v1`. Browser Origins must exactly
match configured HTTPS/HTTP origins; native CLI connections may omit Origin.
Port forwarding requires an injected `PortAuthorizer` to resolve the allowed
port from trusted Workspace state. The client cannot supply a host or arbitrary
destination. The Resource Plane `StreamDialer` receives verified claims and
opens the private runtime connection; the Gateway itself does not proxy bytes
through the Control Plane or query Aurora DSQL. `workspaceagent.GatewayDialer`
is a local adapter for one Workspace's fixed-shell PTY manager and private
ports: it checks the signed Workspace ID, opens `/bin/sh` in that Workspace,
and dials only `127.0.0.1:{port}` after Gateway `PortAuthorizer` approval. It
rejects VS Code until that runtime adapter is supplied. A Resource Plane
router still needs to map signed Workspace claims to the right runtime;
caller-provided hostnames are never accepted. Active session/revocation state
is checked through the verifier's injected `ActiveSessionLookup`; the local
`HTTPActiveSessionLookup` uses the signed HTTPS/HMAC Control Plane endpoint,
which checks the complete claims identity against PostgreSQL and returns only
the active bit. No terminal/application payload is sent to the Control Plane.

The Gateway bounds concurrent total and per-Workspace connections in-process,
and applies a maximum payload size to every incoming WebSocket frame and every
outgoing chunk (64 KiB default, 1 MiB maximum). Stream contents and tickets are
not logged. `/healthz` returns only static readiness status. Limits are
per-process, not fleet-wide. A ticket ID can hold only one concurrent stream
per Gateway process; after that stream closes, the ticket can reconnect until
expiry/revocation. Multiple Gateway replicas still need shared active-connection
coordination and load-balancer/API throttles. TLS termination, private Gateway-to-runtime transport,
and task/service health wiring are deployment responsibilities.

Local WSS tests exercise shell byte relay and a Gateway-to-real-local-PTY
command, a port-forward byte round-trip to an ephemeral `127.0.0.1` listener,
wrong channel/Workspace/Origin, plaintext rejection, denied/allowed port
forwarding, per-Workspace connection cap, concurrent ticket replay rejection
with reconnect-after-close, upstream failure, and oversized frames. The concrete authenticated
Resource Plane multi-Workspace router/configuration, distributed limits, metrics/audit events,
long-running service deployment, IDE process bridge, and Lambda MicroVM
end-to-end stream PoC remain open Task 46/47 work.
