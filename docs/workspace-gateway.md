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
and dials only `127.0.0.1:{port}` after Gateway `PortAuthorizer` approval. The
SSH route has a separate signed permission and always dials `127.0.0.1:22`; it
does not use the caller-controlled port route or public SSH ingress. It
rejects VS Code until that runtime adapter is supplied. A Resource Plane
router still needs to map signed Workspace claims to the right runtime;
caller-provided hostnames are never accepted. Active session/revocation state
is checked through the verifier's injected `ActiveSessionLookup`; the local
`HTTPActiveSessionLookup` uses the signed HTTPS/HMAC Control Plane endpoint,
which checks the complete claims identity against PostgreSQL and returns only
the active bit. `workspaceagent.WorkspaceRouter` is a local in-process router
that selects the registered single-Workspace dialer using the verified
Workspace claim. It rejects duplicate live registrations, and cleanup must
compare against the expected dialer so a delayed removal cannot unregister a
replacement. Unknown Workspace IDs fail closed. Registration is an internal
Resource Plane responsibility; it does not accept host/port destinations and
does not yet discover runtimes or consume lifecycle events. The map is
process-local, so production wiring must register/unregister entries from the
Resource Plane runtime lifecycle and define behavior across Gateway replicas.
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
authorization binding are locally tested, but Workspace lifecycle registration
and a real Lambda endpoint connection remain unverified. This adapter currently
supports shell only; VS Code and Guest-side port-forward channels require
additional runtime endpoints.

The Gateway bounds concurrent total and per-Workspace connections in-process,
and applies a maximum payload size to every incoming WebSocket frame and every
outgoing chunk (64 KiB default, 1 MiB maximum). Stream contents and tickets are
not logged. `/healthz` returns only static readiness status. Limits are
per-process, not fleet-wide. A ticket ID can hold only one concurrent stream
per Gateway process; after that stream closes, the ticket can reconnect until
expiry/revocation. Multiple Gateway replicas still need shared active-connection
coordination and load-balancer/API throttles. TLS termination, private Gateway-to-runtime transport,
and task/service health wiring are deployment responsibilities.

Local WSS tests exercise shell byte relay and a Gateway → WorkspaceRouter →
Workspace-bound PTY command path, dedicated SSH-proxy permission and fixed `127.0.0.1:22` routing, a
port-forward byte round-trip to an ephemeral `127.0.0.1` listener,
wrong channel/Workspace/Origin, plaintext rejection, denied/allowed port
forwarding, per-Workspace connection cap, concurrent ticket replay rejection
with reconnect-after-close, upstream failure, and oversized frames. The concrete authenticated
Resource Plane runtime lifecycle registration, distributed limits, metrics/audit events,
long-running service deployment, IDE process bridge, and Lambda MicroVM
end-to-end stream PoC remain open Task 46/47 work.
