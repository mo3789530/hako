# Workspace CLI connections

Tenant quota usage is available with `hako usage <tenant-id>`. It reports
active Workspaces against the current limit of 10 and concurrent Workload Runs
against the limit of 5. Pending deletion/cancellation remains counted until
runtime cleanup/terminal result is confirmed. This aggregate view does not
change Member visibility of Workspace records and does not report measured
compute or storage consumption. See [Workspace lifecycle API](workspace-lifecycle-api.md).

The first interactive CLI command is:

```sh
hako login
export HAKO_API_URL=https://api.example.test
export HAKO_GATEWAY_URL=https://gateway.example.test
hako wait <tenant-id> <workspace-id> --state running
hako shell <tenant-id> <workspace-id>
hako exec <tenant-id> <workspace-id> -- go test ./...
hako exec <tenant-id> <workspace-id> --timeout 90s -- go test ./...
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
ticket in memory and does not print it. The Guest returns a random PTY session
ID, retained only in the CLI process. If WSS is interrupted, the CLI requests
a fresh authorized Hako Session and one-use Guest ticket, then resumes the same
PTY with bounded retry/backoff. On normal stream completion, input EOF, or
interruption, it attempts to revoke the current session with a bounded cleanup
request. `Ctrl-C` disconnects; terminal EOF (`Ctrl-D`) sends EOT to the remote
shell. A new invocation obtains a new PTY session.

`hako exec` accepts a bounded argv after `--`; it does not parse or invoke a
shell command string. The optional `--timeout` accepts a whole-millisecond
duration from `100ms` to `5m`; without it the Guest uses its 60-second default.
The timeout travels in the authenticated, bounded exec request and does not
change the Workspace-wide runtime limit. The Control Plane issues a distinct short-lived `exec`
Session permission, and the Gateway forwards the WSS stream directly to the
bound Workspace MicroVM Agent. The Guest Agent runs as the non-root Workspace
identity, fixes cwd to `/workspace`, uses a minimal environment, limits runtime
to five minutes and stdout/stderr to 256 KiB each, and kills the process group
on timeout or cancellation. Exit status and captured output are returned to the
CLI. Relative executable paths must resolve within `/workspace`; absolute
image-provided executables are allowed. This route is locally tested, but
deployed Lambda MicroVM image/routing verification remains outstanding.

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

After idle suspension, explicitly resume before opening an interactive
connection. `hako resume <tenant-id> <workspace-id> --wait` waits until the
Control Plane reports the replacement runtime as `running`; omit `--wait` to
return as soon as the asynchronous resume Operation is accepted. Retrying the
resume request with `--idempotency-key <key>` preserves the same operation.
Shell and IDE commands never resume a Workspace implicitly.

`hako delete <tenant-id> <workspace-id>` asks the operator to type the exact
Workspace ID before sending the irreversible delete request. EOF, an empty
answer, or any other text cancels without contacting the API. Automation can
use `--yes`; this bypass is accepted only for `delete`, not `suspend` or
`resume`. An accepted delete remains asynchronous, so use
`hako wait <tenant-id> <workspace-id> --state deleted` to confirm the observed
terminal state. The delete request supports `--idempotency-key <key>` for safe
retries after an ambiguous network result.

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

Copy files through the private Gateway-to-Agent stream:

```sh
hako cp acme ws_api ./config.yaml /workspace/config.yaml
hako cp --from acme ws_api /workspace/logs/app.log ./app.log
```

`hako cp` does not invoke OpenSSH, SSH, or SFTP and does not require guest
`sshd`, an SSH host key, or `known_hosts`. The CLI requests a short-lived,
RBAC-authorized `file-transfer` Session, then streams a bounded binary file
protocol over TLS/WSS through the Gateway to the Workspace Agent. The ticket
is sent only in the WebSocket Authorization header and is revoked after the
transfer (it also expires if revocation cannot reach the API). Files are
restricted to absolute paths under `/workspace`; directory transfers and
paths containing traversal or symlinks are rejected. Uploads are written to
a private temporary file and atomically renamed; downloads use a private local
temporary sibling and rename only after the complete stream arrives. Files are
limited to 1 GiB, although the short-lived Session lifetime can end a slow
transfer earlier. An interrupted upload is discarded and an incomplete
download never replaces the local destination; retry starts the file again.
File contents do not pass through the Control Plane and no
SSH/database/application port is exposed. The actual Lambda MicroVM image and
deployed Gateway-to-Agent route remain runtime verification gates.

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
hako service usage <tenant-id> <workspace-id>
hako service stop <tenant-id> <workspace-id> <instance-name>
hako service start <tenant-id> <workspace-id> <instance-name>
hako service remove <tenant-id> <workspace-id> <instance-name>
hako service rotate-credentials <tenant-id> <workspace-id> <instance-name>
```

`hako add` resolves only a built-in, versioned template and persists a
Workspace-scoped desired-running Service. Repeating the same catalog/name is
safe and returns the existing instance; reusing a name for another template
returns a conflict. `hako services` lists only Services in a Workspace the
caller is authorized to see. It prints desired and observed state, desired and
observed revision, the runtime allocation generation backing the latest report,
and a health column. Health is `pending` unless the API can match the report to
the current active Lambda MicroVM binding and desired revision; therefore a
healthy report from a replaced MicroVM is never presented as current. A
`healthy` value means the Workspace runtime's catalog health check reported
healthy for that current revision, not that the service is externally
reachable or production-ready. An accepted request alone is not proof that a
database is running. No service port is published publicly; access goes through
the Workspace Gateway's authorized loopback port-forward path.

Service admission reserves the catalog-declared memory/CPU request while the
Service is desired-running or a stop/delete is still pending. A Workspace is
limited to the Tenant's configured aggregate Service limits (default and hard
ceiling 2 GiB/1500 millicores); a request exceeding the limit is rejected with
HTTP 409 `service_resource_quota_exceeded`.
The reservation is released only after a runtime report confirms `stopped` or
`deleted`. These are configured admission limits, not measured MicroVM usage
or a guarantee that a runtime class can provide that capacity. Tenant
Owners/Admins can lower the Workspace Service cap through the quota API/CLI;
see [Tenant quota configuration](tenant-quotas.md). Class-specific budgets and
measured usage remain future work.

`hako service usage <tenant-id> <workspace-id>` shows that Workspace's
reserved catalog-request totals. The same data is available from the
authenticated `GET /v1/tenants/{tenant_id}/workspaces/{workspace_id}/services/usage`
route. Workspace Member visibility applies: a Member cannot inspect another
Member's Workspace, and cross-Tenant requests are hidden as not found.

`hako service start|stop|remove` records `running`, `stopped`, or `deleted` as
desired state; observed state remains pending until a matching runtime report
confirms the action. Repeating the same action is idempotent. `remove` leaves a
tombstone and reserves the Service name until runtime cleanup is confirmed. The
local executor removes the container and only its Service-scoped data subtree
before reporting deletion, but deployed Lambda MicroVM/EFS behavior has not
been verified. A deleted Service cannot be restarted or recreated under the
same name. Each changed state is audit-recorded; repeated no-op requests do
not create additional audit events.

`hako service rotate-credentials` is available for a running catalog Service
that declares generated credentials (currently PostgreSQL). It records a new
credential generation and reconciliation revision. The command never returns
the password; see [Workspace Service generated credentials](workspace-service-secrets.md)
for staged promotion and retry behavior.

The Gateway URL must be an HTTPS/WSS origin without a path, query, or embedded
credentials. TLS verification remains enabled. The client constructs the route
from the Workspace ID and the command's fixed permission/channel; callers
cannot choose a runtime hostname or destination port. The SSH route is always
the dedicated `ssh-proxy` channel to port 22.

The shell command uses a bounded binary protocol with attach/session, data,
resize, and terminal-end frames. Both ends decode incrementally because the
Gateway transports a byte stream and does not preserve WebSocket message
boundaries. PTYs can reconnect while the same Guest/MicroVM remains alive;
reconnect across Guest/MicroVM replacement is not provided. Production runtime
provisioning for code-server and SSH remains open. File transfer uses its own
signed Agent route, separate from the SSH capability and confined to the
Workspace mount. The Service Catalog's
revision-fenced Resource Plane consumer, rootless executor, health reporting,
credential handoff, and cleanup are implemented and locally tested. They do
not establish a production persistence guarantee: local development databases
are Workspace runtime services, while real Lambda MicroVM image, EFS, networking,
and resource-limit behavior remain unverified. The local forwarding CLI and
Gateway protocol have a local E2E, but production Resource Plane routing remains
unfinished. Deployment still needs production ticket signing and active-session
lookup key configuration, a multi-Workspace runtime router, and fleet-wide
limits/observability.

Built-in PostgreSQL and Redis images are pinned to their official ARM64 OCI
manifest digests. Version tags are not accepted as runtime identities because
they can be moved; updating an image requires reviewing the release version and
replacing the digest in the Service Catalog. Digest pinning does not by itself
verify the image at runtime or prove Lambda MicroVM compatibility.

The Workload Agent and Workspace init MicroVM Dockerfiles also pin the ARM64
Lambda MicroVM base by child-manifest digest. The Workload Agent pins its Go
toolchain image the same way. A base-image update must review the upstream tag,
platform manifest, and digest together; local packaging still does not prove an
AWS MicroVM build or runtime.

## Security and failure behavior

- Missing login credentials or required endpoint configuration fail before a
  Gateway stream is established.
- Ticket issuance is tenant/workspace RBAC protected by the API; a denied API
  request fails without opening the Gateway stream.
- TLS certificate validation is mandatory. Tickets are not included in the
  URL, command line, or normal CLI output.
- A Gateway disconnect causes the CLI to reauthorize and reconnect to the same
  PTY. Retries are bounded; if recovery fails, the command exits with an error.
  The client attempts session revocation when the stream exits, but a network
  failure can prevent the revoke request from reaching the API; tickets
  therefore also have a short expiration. Reconnect cannot survive Guest Agent
  or MicroVM replacement.
- `hako shell` opens no local listening port. `hako forward` binds exclusively
  to `127.0.0.1`, so it does not publish a Workspace service to the LAN or
  internet.

Local authenticated integration coverage exercises API ticket creation, a TLS
WSS shell round trip, API revocation, authorization denial before Gateway
connection, cancellation-triggered stream close/revoke, loopback TCP
forwarding, per-connection session revocation, invalid-port rejection, and
missing Gateway configuration. Guest tests cover same-user PTY reattachment,
cross-user/concurrent attach rejection, and output produced while detached.
It does not substitute for a deployed
MicroVM/PTY, production key-management, or multi-replica Gateway test.
