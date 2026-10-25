# Workspace Agent shell and PTY

`internal/workspaceagent` is the interactive Workspace shell runtime. It is
separate from `internal/workloadagent`: CI Workloads retain their fixed,
server-owned command and attempt-scoped tickets, while a developer shell is an
interactive capability authorized by a Workspace session.

## Current local implementation

Non-interactive `exec` is connected locally from CLI through the Control Plane
session API and TLS Gateway to the Workspace Guest Agent. `hako exec <tenant>
<workspace> -- <argv...>` requests a short-lived Session with only the separate
`exec` permission, sends argv as strict bounded JSON over WSS, and revokes the
Session on completion or interruption. It is an argv protocol, not a shell
string; callers that need shell syntax must explicitly run a shell executable.
The Agent uses the fixed Workspace root and minimal `HOME`/`PATH`/`LANG`/`TERM`
values, starts the process group as the already-unprivileged Workspace Agent,
caps each output stream at 256 KiB, and limits execution to five minutes.
Timeout or caller cancellation kills the process group. Relative executable
paths must resolve under `/workspace`; absolute image executables are allowed.
Signed one-use Agent Tickets bind the operation to the current MicroVM and
`exec` permission. Unit tests cover process limits and environment isolation;
Guest WSS tests cover dedicated permission, actual command output, and replay
rejection; CLI/Gateway tests cover session issuance, output, and revocation.
The deployed MicroVM image and live runtime route are not yet verified.

The private unprivileged Agent also exposes a versioned heartbeat response
only on its root-owned Unix socket. Root init renews a 30-second liveness lease
every 10 seconds and checks a fresh response plus its
Tenant/Workspace/Resource Plane/MicroVM binding before forwarding each new
Gateway request. A missing, malformed, unsupported-version, or foreign-binding
heartbeat does not renew the lease; after expiry the supervisor fails closed
with HTTP 503 until a valid heartbeat recovers. This is a Guest-process lease,
not a Control Plane observed-state update. Lambda MicroVM lifecycle remains
authoritative for observed runtime state.

The Linux PTY manager starts only the image-owned `/bin/sh -i`; callers cannot
select a binary, arguments, environment, or working directory. The root is an
absolute directory supplied by the image/runtime composition and must already
exist. The manager accepts a fixed UID/GID for the shell process. A root guest
init may use this to drop shells to the Workspace EFS Access Point's POSIX
identity; it rejects UID/GID 0. An unprivileged manager may use only its own
identity. The shell starts there with a minimal `HOME`, `PATH`, `TERM`, and
`LANG` environment. Each session receives a cryptographically random 192-bit
ID and has a configurable lifetime (default eight hours, maximum 24 hours).
The manager limits concurrent sessions (default four, maximum 64), terminal
dimensions (500 columns × 500 rows), and individual input frames (64 KiB).
Resizing uses the PTY window-size ioctl. The CLI/Guest protocol begins with an
attach control frame; the Guest returns the opaque session ID, which the CLI
keeps only in memory. On a dropped WSS stream, the Guest detaches the connection
and keeps the PTY process alive. The same Hako user can reattach with a fresh
one-use Agent Ticket. A concurrent attachment or different user is rejected.
PTY output is held in a 1 MiB rolling buffer so output produced while detached
can be read after reconnect; overflow discards the oldest bytes. The manager's
legacy `Open` primitive remains context-bound for local callers, while
authenticated WSS shells use `OpenOwned` and are independent of the HTTP
request context. Manager shutdown and TTL expiry terminate the process group;
completed sessions cannot be reattached.

The manager remains a local runtime primitive, not a public API. The
`workspaceagent.GatewayDialer` adapts it to the authenticated Gateway and
checks that the signed Workspace ID matches the Workspace-local manager before
opening a shell. A local TLS WSS E2E sends a command through that adapter to a
real PTY and verifies the output. This adapter is currently single-Workspace;
the Resource Plane router must select it from verified claims. The adapter
also supports a port stream only to `127.0.0.1:{port}` after the Gateway's
trusted port allowlist authorizes it; it does not accept a destination host.
The local `workspaceagent.GatewayDialer` has no VS Code backend; the separate
Lambda MicroVM adapter and fixed Guest bridge are documented below.
Reconnection is bounded by the Workspace Agent and MicroVM lifetime; a
replacement MicroVM does not inherit PTYs. The opaque PTY session ID is not
authorization: every attach also requires a fresh ticket bound to user, Tenant,
Workspace, Resource Plane, MicroVM, and shell permission. No public SSH or
additional TCP listener is introduced by this package. The Lambda MicroVM
remains the isolation boundary; this does not claim filesystem confinement
inside the guest.

The guest-side shell endpoint contract is now defined at
`GET /v1/workspaces/{workspace_id}/shell` using the
`hako.workspace.agent.v1` WebSocket subprotocol. It requires an exact configured
Gateway Origin and a one-use, two-minute Ed25519 Agent Ticket distinct from the
browser Session Ticket. The ticket binds user, Tenant, Workspace, Resource
Plane, MicroVM ID, and `shell` permission. The strict `/run` hook provides the
Workspace identity and provider MicroVM ID; the agent binds these only after
the EFS mount succeeds, before `/run` is acknowledged. The handler is covered
by signed-ticket-to-real-PTY tests, including cross-Workspace/MicroVM, origin,
and replay rejection. After EFS mount succeeds, `hako-workspace-init` now
launches `hako-workspace-agent` as a separate process with the Access Point
UID/GID, a minimal environment, no supplementary groups, and no AWS
credentials. The child receives only strict Workspace/MicroVM hook identity on
stdin and an inherited listener on a root-owned mode-0600 Unix socket. Root
init proxies only `/v1/workspaces/...` requests to that socket and waits for an
internal readiness probe before acknowledging `/run`; orderly shutdown stops
the child before EFS unmount. The image build now requires a public-key JSON
file and the pinned EFS RPM. `HAKO_GATEWAY_ORIGIN` remains a required runtime
configuration value.

The Guest Agent also exposes `/vscode` with an independently authorized,
one-use ticket carrying only the `vscode` permission. It forwards bytes only
to the fixed guest-loopback `127.0.0.1:8081`; the caller cannot choose a Host
or port. The Lambda MicroVM Gateway adapter resolves this route only after
Session authorization. After the EFS Access Point identity is bound, the
unprivileged Agent starts the image-pinned ARM64 code-server, binds it to that
loopback address, and waits for `/healthz` before reporting Agent readiness.
It uses `--auth none` because the only ingress is the authenticated Hako
Gateway tunnel; no guest TCP port is exposed. The process uses the Workspace
UID/GID, fixed `/workspace` root, disabled telemetry, and Workspace-persisted
user-data/extensions paths. Agent shutdown terminates the code-server process
group. `hako open` binds locally only to `127.0.0.1` and requests a fresh
Session Ticket per browser TCP stream.

The CLI/Agent shell stream uses an application-level binary frame with the
`HAKS` magic, a type byte, and a four-byte big-endian payload length. Attach
and session frames create/resume a terminal and return its random ID; data
frames carry terminal bytes; resize frames carry two big-endian `uint16`
dimensions, each limited to 1–500; an end frame reports normal shell exit.
Each attachment is generation-fenced so a late goroutine from a disconnected
stream cannot read or write after a new stream attaches. The incremental decoder accepts arbitrary
stream fragmentation/coalescing because the Gateway tunnels bytes and does not
preserve WebSocket message boundaries. Frame buffering is bounded to 64 KiB.
On a local Linux TTY, `hako shell` disables echo and canonical line buffering,
sends the initial window size, forwards `SIGWINCH` updates, and restores the
original termios settings on exit. Piped CLI I/O does not alter terminal state.
After transport loss, `hako shell` creates a fresh authorized Hako Session and
reattaches to the same PTY with bounded retry/backoff. Terminal bytes still do
not pass through the Control Plane. Normal terminal exit, local interruption,
Guest TTL expiry, and MicroVM loss end the corresponding terminal session.

The supervisor, readiness, route restriction, socket permissions, binding,
shutdown, and strict child-payload bounds have local tests. This does not verify
the actual Lambda MicroVM image build, `/dev/pts`, or run-hook behavior. The
Resource Plane now has a local `LambdaMicroVMGatewayDialer`: it checks the
verified Tenant/Workspace/Resource Plane and shell permission, mints a fresh
one-use Agent Ticket and an AWS auth token limited to this MicroVM's port 8080,
then opens the fixed Guest shell WSS route. It sends AWS authentication and
port selection using the `lambda-microvms.authentication.*` and
`lambda-microvms.port.8080` WebSocket subprotocols (AWS removes these before
forwarding) and keeps the Hako Agent Ticket in the `Authorization` header.
Endpoint host
validation is pinned to `{microvm-id}.lambda-microvm.{region}.on.aws`; caller
input cannot choose an address. The SDK token provider, port/expiry scope,
ticket binding, fixed route, AWS subprotocols, and rejection paths have local
tests. `WorkspaceLambdaMicroVMStreamDirectory` now resolves the current
allocation from the Resource Plane journal and verifies provider state for
each new shell stream, removing the need for process-local lifecycle
registration. The same adapter now routes the dedicated `ssh-proxy` permission
to the fixed Guest `/ssh` WebSocket route, where the unprivileged Agent dials
only `127.0.0.1:22`; the caller cannot supply a destination. Local tests cover
the ticket-bound raw-byte bridge and reject arbitrary ports/replay. Workspace
init starts image-installed `sshd` on loopback with public-key-only
authentication and no root/password login, and waits for its listener before
reporting the Agent runtime ready. An EFS-backed per-Workspace host
key is copied to root-only `/run` for sshd and removed at shutdown; retrieve
its public half with authenticated `hako cp --from` and add it to
`known_hosts`. This does not verify an ARM64 image build or live MicroVM route.
SSH host-key provisioning is not needed for `hako cp`, which uses a separate
signed Agent file-transfer route. The
route requires a one-use `file-transfer` Agent Ticket, confines paths beneath
the pinned `/workspace` directory with `openat`/`O_NOFOLLOW`, rejects symlinks
and non-regular files, caps transfers at 1 GiB, and commits uploads by fsync
and atomic rename. The CLI writes downloads to a mode-0600 temporary file and
renames it only after the complete file arrives. Approved port forwarding now uses the
same fixed Guest endpoint: after the Gateway's PortAuthorizer approves the
target, a one-use Agent Ticket binds that exact port and the Guest dials only
`127.0.0.1:<port>`. The Guest rejects non-canonical or mismatched route/ticket
ports before dialing; no arbitrary host is accepted. Local tests verify the
raw-byte relay and port-confusion rejection. Production Gateway composition,
AWS IAM/configuration, and a real MicroVM connection remain unverified.
Production key rotation/distribution is unresolved. Keep Workspace runtime
exposure disabled until deployment gates are complete.

Build the dedicated image with
`make build-workspace-microvm-image HAKO_EFS_UTILS_RPM_VERSION=<pinned-version> HAKO_OPENSSH_RPM_VERSION=<pinned-version> HAKO_NERDCTL_FULL_ARCHIVE=<reviewed-linux-arm64-tar.gz> HAKO_NERDCTL_FULL_VERSION=<pinned-version> HAKO_NERDCTL_FULL_SHA256=<reviewed-sha256> HAKO_CODE_SERVER_VERSION=4.141.0 HAKO_CODE_SERVER_SHA256=e5cc78da1d631c6cf6162f9b18eeaa8ee9d3239e1a88bb99d0602cdc5fa1852d HAKO_WORKSPACE_AGENT_TRUSTED_KEYS_FILE=<public-keys.json> HAKO_PREVIEW_TICKET_KEY_ID=<matching-key-id>`.
The code-server archive is fetched from the upstream GitHub release and
verified against the supplied ARM64 SHA-256 before it is copied into the
runtime image. Keep both version and digest pinned together when updating it.
The JSON contains only key-ID to unpadded base64url Ed25519 public-key values;
the target validates every encoded key and fails when the file is absent or
empty. For Preview-enabled images, set `HAKO_PREVIEW_TICKET_KEY_ID` to the same
ID configured on the Resource Plane Preview Controller and Workspace Gateway;
the target fails if that ID is absent from the Guest's public-key set. Terraform
also requires the Gateway and Preview Controller to share the same key ID and
exact private-key Secret ARN. Configure
`HAKO_GATEWAY_ORIGIN` in the trusted Resource Plane MicroVM launch environment
to the exact HTTPS origin; the guest rejects tickets presented with another
Origin. The root init does not start when this setting or the image-owned key
file path is missing. Do not place the Agent signing private key in this image.
The `nerdctl-full` archive is supplied by the caller and checked against its
required SHA-256 and embedded version before its binaries are copied into the
image. `slirp4netns` is a separate static ARM64 release binary because
`nerdctl-full` does not bundle it; the build requires its version and
SHA-256-pinned input independently. The build does not fetch a moving release.
The image packages setuid `newuidmap`/`newgidmap` helpers. Service execution is
opt-in with `HAKO_WORKSPACE_SERVICES_ENABLED=true` at image build time; the
default is `false`. When enabled, root init prepares a per-MicroVM account and
non-overlapping subordinate UID/GID range, then starts `containerd-rootless.sh`
as the EFS Workspace owner. Root init owns the daemon lifecycle and passes only
its fixed `/run/user/{uid}/containerd/containerd.sock` path to the unprivileged
Agent child. Containerd state is kept in the MicroVM's ephemeral `/tmp`, while
the private runtime socket/state lives on the existing mode-0700 tmpfs;
catalog volumes remain under that Workspace's `/workspace/.hako` subtree. The
process uses RootlessKit with slirp4netns, IPv6 disabled, and the native
snapshotter. Rootless user namespaces, slirp networking, kernel support,
cgroup/resource-limit enforcement, and the Lambda MicroVM image must pass the
runtime PoC before enabling the flag in a deployed image. Slirp currently
allows outbound network access and is not an egress policy; do not treat it as
tenant egress isolation.
The AWS [CreateMicrovmAuthToken API](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_CreateMicrovmAuthToken.html)
supports one or more allowed ports and a 1–60 minute expiration; Hako requests
only port 8080 for two minutes. AWS documents the required base protocol and
authentication/port subprotocols in [Connecting to a MicroVM](https://docs.aws.amazon.com/lambda/latest/dg/microvms-launching.html#connecting-to-a-microvm).

## Verification

`GOCACHE=/tmp/hako-go-cache go test -race ./internal/workspaceagent ./internal/workspacegateway -count=1`
exercises a real local Linux PTY, shell input/output, resize, environment and
working-directory constraints, input bounds, session limits, completed-session
rejection, explicit shutdown, process-group cancellation, TLS WSS-to-PTY
command/output, and port-forward relay to a loopback listener. These local tests do not establish that the Lambda MicroVM
image has `/dev/ptmx` and `/dev/pts` mounted correctly; that remains part of
the image/runtime PoC.

## Workspace Service runtime (Task 84, local contract)

The Guest Agent has an optional `POST /v1/workspaces/{workspace_id}/services/reconcile`
route. It is disabled unless the process is composed with a Service engine. The
route requires a one-use Ed25519 capability binding Tenant, Workspace,
Resource Plane, MicroVM ID, Service ID, desired revision/state, credential
generation, catalog version, and the exact configuration digest. The request body is bounded and strict;
only exact built-in PostgreSQL/Redis catalog JSON is accepted. The privileged
root init process verifies the ticket and atomically persists capability replay
IDs and each Service's highest accepted revision under its private `/run`
directory before proxying to the unprivileged Agent child. Thus an Agent child
restart in the same MicroVM does not reopen stale revisions or replayed
capabilities; state is discarded when a new MicroVM identity is bound. It is
not a cross-MicroVM durable cleanup ledger.

An optional Linux `nerdctl` adapter uses the rootless containerd socket provided
by `HAKO_CONTAINERD_SOCKET` and the image-owned binary at
`/usr/local/bin/nerdctl`. It uses only fixed catalog image/argv, memory/CPU bounds,
Workspace-private networking, and guest-loopback port mappings; its process
environment excludes ambient AWS and other credentials. Persistent catalog
volumes are placed below `/workspace/.hako/services/` with symlink escape
checks. Removing a Service first removes its container and then recursively
removes only that Service ID's data subtree using descriptor-relative,
no-follow filesystem operations. If either step fails, the Guest reports
failure; the Control Plane must retain the deletion tombstone and retry rather
than releasing the name as deleted. This cleanup is scoped to the current
Workspace filesystem and is not a durable cross-MicroVM cleanup ledger.
PostgreSQL receives a generated per-Service password through the Resource Plane
controller and authenticated Guest request; it is never stored in DSQL,
queues, process arguments, image layers, or logs. `hako service
rotate-credentials` stages a generation-specific Secret version, updates the
PostgreSQL role through fixed `psql` stdin, and promotes `AWSCURRENT` only after
the Guest reports healthy for the current revision and MicroVM generation.
Credential rotation and retirement are local/fake tested; real Secrets Manager
and MicroVM execution have not been verified. The MicroVM image build can include a
caller-pinned `nerdctl-full` bundle and has a default-off opt-in for a
root-init-supervised rootless containerd process whose fixed socket is passed
into the Agent child. Local fake-runner tests verify command construction and
fail-closed behavior. The opt-in rootless daemon lifecycle is wired in the
image init/supervisor, but a real Lambda MicroVM run has not demonstrated
container startup, user-namespace/network behavior, or cgroup resource limits.
The Resource Plane command consumer and default-off Terraform queue/Lambda/IAM
wiring are implemented and mock-plan validated, but have not been applied to
AWS.

The Control Plane also exposes
`POST /v1/internal/resource-planes/{plane}/workspace-services/{service}/reports`
for HMAC-authenticated, size-bounded runtime observations. Report IDs are
idempotent and cannot be reused with different content. The database applies
an observation only when Tenant, Workspace, Placement, current
`lambda_microvm` binding, desired revision, and desired state all match;
stale or foreign-MicroVM reports are acknowledged with `accepted:false` and
cannot overwrite newer state. PostgreSQL integration tests cover accepted,
duplicate, conflicting-ID, foreign-MicroVM, and stale-revision reports. The
local Resource Plane consumer now reads the durable Workspace journal, checks
that the Lambda MicroVM is still running with its pinned image/network profile,
requests a two-minute auth token for Guest port 8080 only, issues the signed
one-use Service capability, calls the fixed Guest endpoint, and posts an HMAC
report. Its SQS Lambda adapter returns per-record failures for retry. This
consumer now has a Lambda executable that composes the S3 journal, Lambda API,
mounted Ed25519 ticket key, and mounted HMAC report key. Configure
`HAKO_WORKSPACE_SERVICE_TICKET_KEY_ID`,
`HAKO_WORKSPACE_SERVICE_TICKET_KEY_SECRET_ARN`,
`HAKO_WORKSPACE_SERVICE_REPORT_ENDPOINT`, and
`HAKO_WORKSPACE_SERVICE_REPORT_KEY_SECRET_ARN` alongside the Resource Plane
and Workspace journal settings. These must be distinct, exact Secrets Manager
ARNs in the Resource Plane Region. The Lambda role reads only these two ARNs at
cold start; key rotation therefore requires a Lambda environment restart, and
ticket-key rotation also requires the matching Guest public key to be trusted.
Local tests use synthetic keys and do not retrieve Secrets. Before issuing a
Guest capability, the consumer performs a timestamped HMAC-authenticated lookup
of the authoritative Service revision/state/configuration from the Control
Plane. SQS records are wake-up signals: an old queued start after a newer stop
causes the consumer to apply the latest stop, not the stale payload. The lookup
hides foreign Tenant/Workspace/Placement targets and returns only the
non-secret desired-state command. This prevents stale queued work after
MicroVM replacement; a concurrently arriving new state still relies on
SQS redelivery for convergence: after the Guest call the consumer re-resolves
desired state and withholds the old report if the revision changed. The
side effect itself is not transactionally serialized with Control Plane state,
so a transient old state is still possible. Build the ARM64 ZIP
with `make build-workspace-service-controller-lambda`; Resource Plane Terraform
has default-off queue/event-source/IAM/alarm wiring validated with a mock plan,
but no resources have been applied. The MicroVM image still lacks
containerd/nerdctl, and report acceptance does not confirm durable volume
cleanup or release Service tombstones.
