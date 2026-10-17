# Workspace CLI connections

The first interactive CLI command is:

```sh
hako login
export HAKO_API_URL=https://api.example.test
export HAKO_GATEWAY_URL=https://gateway.example.test
hako shell <tenant-id> <workspace-id>
```

`hako shell` uses the saved Cognito access token to ask the Control Plane for a
short-lived shell-only Hako Session Ticket. The Control Plane applies its normal
tenant/workspace authorization before issuing the ticket. The CLI then opens a
TLS WebSocket directly to the configured Gateway and relays terminal bytes;
terminal content does not pass through the Control Plane. The CLI keeps the
ticket in memory and does not print it. On normal stream completion, input EOF,
or interruption, it attempts to revoke the session with a bounded cleanup
request. `Ctrl-C` disconnects; terminal EOF (`Ctrl-D`) sends EOT to the remote
shell. A new invocation obtains a new ticket.

Forward one allowed Workspace loopback service port with:

```sh
hako forward <tenant-id> <workspace-id> <remote-port> [local-port]
```

When `local-port` is omitted it matches `remote-port`; use `0` for an ephemeral
local port and read the selected port from the CLI's status line. The listener
always binds to `127.0.0.1`, never a public or LAN interface. Each accepted TCP
connection obtains its own port-forward-only ticket and WSS stream, then
attempts to revoke that ticket when the local connection closes. A single CLI
process allows at most 16 simultaneous forwarded connections. The remote port
is authorized against trusted Workspace service state; the CLI cannot provide
a destination host.

Add and inspect Workspace-local catalog Services:

```sh
hako add <tenant-id> <workspace-id> postgres [instance-name]
hako add <tenant-id> <workspace-id> redis [instance-name]
hako services <tenant-id> <workspace-id>
hako service stop <tenant-id> <workspace-id> <instance-name>
hako service start <tenant-id> <workspace-id> <instance-name>
hako service remove <tenant-id> <workspace-id> <instance-name>
```

`hako add` resolves only a built-in, versioned template and persists a
Workspace-scoped desired-running Service. Repeating the same catalog/name is
safe and returns the existing instance; reusing a name for another template
returns a conflict. `hako services` lists only Services in a Workspace the
caller is authorized to see. The current API reports `observed_state=pending`:
this metadata flow does not yet launch or health-check containers in the
Lambda MicroVM. Do not treat an accepted request as a running database. No
service port is published publicly; access is intended to go through the
Workspace Gateway's authorized loopback port-forward path once the runtime
router consumes Service state.

`hako service start|stop|remove` records `running`, `stopped`, or `deleted` as
desired state and returns `observed_state=pending`. Repeating the same action
is idempotent. `remove` leaves a tombstone and reserves the Service name; it is
not evidence that a container or volume has been physically deleted. A deleted
Service cannot be restarted or recreated under the same name. Each changed
state is audit-recorded; repeated no-op requests do not create additional
audit events.

The Gateway URL must be an HTTPS/WSS origin without a path, query, or embedded
credentials. TLS verification remains enabled. The client constructs the route
from the Workspace ID and the fixed `shell` permission; callers cannot choose a
runtime hostname or destination port.

The shell command is currently a single-stream transport. It does not yet
provide a reconnectable PTY, terminal resize signaling, SSH ProxyCommand/VS Code
Remote SSH, or file transfer. The Service Catalog API/CLI currently persists
desired state only; runtime start/health/stop/remove execution and
persistent-data behavior are not yet implemented. The local forwarding CLI and Gateway protocol have a local
E2E, but production Resource Plane routing remains unfinished. Deployment
still needs production ticket signing
and active-session lookup key configuration, a multi-Workspace runtime router,
and fleet-wide limits/observability.

## Security and failure behavior

- Missing login credentials or required endpoint configuration fail before a
  Gateway stream is established.
- Ticket issuance is tenant/workspace RBAC protected by the API; a denied API
  request fails without opening the Gateway stream.
- TLS certificate validation is mandatory. Tickets are not included in the
  URL, command line, or normal CLI output.
- A Gateway disconnect ends the CLI stream; automatic reconnect is not
  implemented. The client attempts session revocation when the stream exits,
  but a network failure can prevent the revoke request from reaching the API;
  tickets therefore also have a short expiration.
- `hako shell` opens no local listening port. `hako forward` binds exclusively
  to `127.0.0.1`, so it does not publish a Workspace service to the LAN or
  internet.

Local authenticated integration coverage exercises API ticket creation, a TLS
WSS shell round trip, API revocation, authorization denial before Gateway
connection, cancellation-triggered stream close/revoke, loopback TCP
forwarding, per-connection session revocation, invalid-port rejection, and
missing Gateway configuration. It does not substitute for a deployed
MicroVM/PTY, production key-management, or multi-replica Gateway test.
