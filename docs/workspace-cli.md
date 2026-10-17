# Workspace CLI connections

The first interactive CLI command is:

```sh
hako login
export HAKO_API_URL=https://api.example.test
export HAKO_GATEWAY_URL=https://gateway.example.test
hako wait <tenant-id> <workspace-id> --state running
hako shell <tenant-id> <workspace-id>
hako open <tenant-id> <workspace-id> [local-port]
hako ssh-proxy <tenant-id> <workspace-id>
hako cp <tenant-id> <workspace-id> <local-path> <workspace-path>
hako cp --from <tenant-id> <workspace-id> <workspace-path> <local-path>
```

`hako shell` uses the saved Cognito access token to ask the Control Plane for a
short-lived shell-only Hako Session Ticket. The Control Plane applies its normal
tenant/workspace authorization before issuing the ticket. The CLI then opens a
TLS WebSocket directly to the configured Gateway and relays terminal bytes;
terminal content does not pass through the Control Plane. When stdin and
stdout are the local Linux terminal, the CLI disables local echo/canonical
line buffering, sends the terminal's initial size, tracks `SIGWINCH`, and
restores the original terminal settings on exit. Piped input/output remains
unchanged. The CLI keeps the
ticket in memory and does not print it. On normal stream completion, input EOF,
or interruption, it attempts to revoke the session with a bounded cleanup
request. `Ctrl-C` disconnects; terminal EOF (`Ctrl-D`) sends EOT to the remote
shell. A new invocation obtains a new ticket.

`hako open` starts a local listener bound only to `127.0.0.1`, opens the
browser at that URL, and forwards each browser TCP connection through a
short-lived `vscode` Session Ticket and the Gateway's fixed VS Code route.
The Guest Agent only connects to `127.0.0.1:8081`; it accepts no browser
destination/port. The Workspace image/runtime must start code-server on that
loopback address and configure its own authentication. Hako does not disable
or replace code-server authentication. `Ctrl-C` closes the listener and
active streams; each stream's Hako session is revoked after disconnect.

Workspace lifecycle API requests are asynchronous: an accepted create,
suspend, resume, or delete request is not proof that the observed state has
changed. `hako wait <tenant-id> <workspace-id>` polls the authorized Workspace
GET endpoint until the observed state becomes `running` (the default).
`--state suspended` and `--state deleted` select the other stable targets.
Progress is printed only when the observed state changes. It exits immediately
on `failed`, on deletion while waiting for another target, API errors, or
Ctrl-C. The default timeout is 10 minutes and can be changed up to 30 minutes
with `--timeout`; `--interval` defaults to 2 seconds and is bounded between
250 milliseconds and 30 seconds. This command reports the Control Plane's
observed state; it does not make the Lambda MicroVM runtime available where
the Resource Plane executor is not deployed.

Configure OpenSSH or VS Code Remote SSH to use Hako as a ProxyCommand:

```sshconfig
Host hako-ws-api-dev
    HostName hako-workspace
    User hako
    ProxyCommand hako ssh-proxy <tenant-id> <workspace-id>
```

`hako ssh-proxy` gets a short-lived ticket with the dedicated `ssh-proxy`
permission and relays raw SSH bytes over TLS/WSS. The Gateway routes this
permission only to the fixed Workspace-local `127.0.0.1:22`; the client cannot
choose a host or port. TCP/22 remains private and is never exposed publicly.
The SSH daemon must be present and configured inside the Workspace image. The
ProxyCommand emits no protocol text on stdout; diagnostics go to stderr. It
attempts session revocation when the stream closes or the process is cancelled.
The Lambda MicroVM adapter and unprivileged Guest Agent now implement the
dedicated `/ssh` WSS bridge and fixed `127.0.0.1:22` dial, with one-use
MicroVM-bound authorization. This does not install the SSH daemon or provide a
host key; those remain image/runtime provisioning gates.

Copy files through the same private SSH tunnel:

```sh
hako cp acme ws_api ./config.yaml /workspace/config.yaml
hako cp --from acme ws_api /workspace/logs/app.log ./app.log
```

`hako cp` delegates transfer to the system OpenSSH `scp` client and requires
OpenSSH 9.0 or newer so transfers use SFTP rather than the legacy remote-shell
protocol. It configures the Hako ProxyCommand plus a unique
`HostKeyAlias` for the Tenant/Workspace pair. The remote path must be absolute
and remain lexically under `/workspace`; the command never accepts a remote
host. Normal SSH host-key checking remains enabled: verify and provision the
Workspace host key in `known_hosts` before unattended copies. The SSH daemon
must expose the SFTP subsystem. No file contents pass through the Control
Plane, and the CLI does not add credential flags or place Hako tickets in the
scp argument list. The actual MicroVM sshd/SFTP and host-key provisioning are
still runtime deployment gates.

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
from the Workspace ID and the command's fixed permission/channel; callers
cannot choose a runtime hostname or destination port. The SSH route is always
the dedicated `ssh-proxy` channel to port 22.

The shell command is currently a single-stream transport. Its bounded binary
protocol uses a magic/type/length header and typed data/resize frames; both
ends decode incrementally because the Gateway transports a byte stream and
does not preserve WebSocket message boundaries. It does not yet provide a
reconnectable PTY, code-server image/runtime startup, or production SSH/SFTP
runtime provisioning. File transfer uses the local OpenSSH `scp` client
through the same unverified MicroVM SSH runtime path. The Service Catalog API/CLI currently persists
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
