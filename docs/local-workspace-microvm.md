# Local Workspace MicroVM Development

Status: host-level Firecracker boot, the concrete local Firecracker driver's
ensure/guest-orderly-shutdown/controller-reconstruction/cold-resume/delete
lifecycle, and ext4 `/workspace` persistence across VM restart are repeatable. The durable
local journal and Resource Controller orchestration layer are implemented.
The host-native Hako Workspace OCI rootfs and initrd build are now available.
The host-native initrd boots under KVM, mounts its persistent ext4 disk, starts
the Workspace Agent as UID/GID 100000, and answers its identity-bound HTTP
heartbeat over vsock. Ensure and Resume wait for that identity-bound heartbeat
before reporting `running`; Suspend waits for Guest filesystem quiescence.
Orderly Guest shutdown and cleanup pass. A private identity-bound Guest control
request on vsock port 7001 asks PID 1 to stop the Agent, sync and unmount the
persistent disk, and report quiescence before shutdown. An initrd root
directory accidentally archived with mktemp mode 0700 caused the earlier EACCES;
the packager now sets 0755 and tests the newc root entry. Local operation
API is connected to the existing Resource Controller contract over a private
same-user Unix socket, and an end-to-end KVM operation smoke covers Ensure,
Suspend, cold Resume, and Delete. The local Outbox worker now connects the
normal PostgreSQL Operation flow to the local Controller without SQS or AWS.
The local loopback OAuth/JWKS provider, API HTTPS and private Session-key
configuration, Outbox worker, Gateway key bundle generator, and private
Firecracker launch-profile generator are implemented. Authenticated
`login/create/wait running/delete/wait deleted` passed against a real local
Guest on 2026-11-23. The signed-vsock smoke now passes exec, PTY shell, upload/download, Chromium
rendering of the VS Code page, SSH public-key authentication and command
execution, and file byte equality after cold resume. The controller smoke also
passes restart and exact Resume replay with the same allocation. Interactive routing through the real Gateway and Guest now passes for exec,
files, shell, VS Code and SSH. The Hako CLI interactive command itself remains
under acceptance.
The Workspace Gateway can select a host-local journal and route
authorized streams to the active Guest over vsock without loading AWS
configuration. API local Session signing, Resource Plane HMAC lookup, HTTPS,
the local OAuth/JWKS provider, key generation, and private launch-profile
generation are implemented and focused-tested. A first real-Guest
authenticated CLI run now passes login, create, waiting for `running`, delete,
and waiting for `deleted`. The initial interactive failure was caused by carrying the HTTP request context
past WebSocket upgrade; the Agent now scopes stream work to the WebSocket
handler. Signed-vsock smoke passes exec, PTY shell, upload/download, and
same-disk file persistence across cold resume. Chromium renders the VS Code
page through the authorized Guest bridge; OpenSSH authenticates with a temporary
key and executes `id -u` as UID 100000. A real controller smoke restarts after
Resume, replays the exact operation, confirms the same allocation ID, then
Deletes it. Hako CLI `exec` now passes through a real API Session and Gateway.
Task 97 repository install/readback, identical retry, and local-edit preservation
also pass through this Guest. The remaining Task 98 acceptance is the complete
interactive shell CLI workflow. See [Task 98](../tasks/task-98-local-workspace-microvm.md).

## Compose the local authenticated Control Plane and Gateway

This profile uses the normal Hako API authorization and PKCE login, local
PostgreSQL, a loopback-only OAuth/JWKS identity provider, the PostgreSQL
Workspace Outbox worker, the Firecracker reconciler, and the AWS-free local
Gateway. The API verifier accepts the local issuer only on loopback HTTP. The
development identity provider binds only to `127.0.0.1` or `::1`, always issues
the one configured local subject, and is not suitable for a shared host. API
and Gateway traffic remains HTTPS using the generated `localhost` certificate.

Build the local processes and create a new private key directory. The
generator refuses to overwrite an existing directory and writes owner-only
files:

```sh
make build-local-workspace-stack
build/hako-local-workspace-keygen -dir /tmp/hako-local-workspace-keys
```

Apply Hako migrations and seed one local Tenant owner plus the active
Firecracker Resource Plane:

```sh
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
make migrate
psql "$HAKO_DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/seed-local-workspace.sql
```

Trust `gateway-cert.pem` in the local OS/browser trust store before opening the
HTTPS API or Workspace Gateway. Then run each process in its own terminal, as
the same Unix user. The API and Gateway must use the same Session HMAC key and
Resource Plane ID. The Guest initrd must contain the generated `agent-public.json`
content at `/etc/hako/workspace-agent-public-keys.json`. Put the key in the
extracted host-native image rootfs before packaging it (the rootfs also needs
the pinned code-server, OpenSSH, and matching host kernel modules):

```sh
GOOS=linux GOARCH="$(go env GOARCH)" CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o /tmp/hako-workspace-agent ./cmd/hako-workspace-agent
install -m 0755 /tmp/hako-workspace-agent \
  /path/to/workspace-rootfs/usr/local/bin/hako-workspace-agent
install -m 0644 /tmp/hako-local-workspace-keys/agent-public.json \
  /path/to/workspace-rootfs/etc/hako/workspace-agent-public-keys.json
HAKO_LOCAL_WORKSPACE_ROOTFS_DIR=/path/to/workspace-rootfs \
HAKO_LOCAL_WORKSPACE_INCLUDE_HOST_MODULES=true \
make build-local-workspace-initrd \
  FIRECRACKER_KERNEL_CONFIG="/boot/config-$(uname -r)" \
  FIRECRACKER_VSOCK_MODULE_DIR="/lib/modules/$(uname -r)"
```

Start the local identity provider:

```sh
HAKO_LOCAL_IDP_ADDR=127.0.0.1:8787 \
HAKO_LOCAL_IDP_CLIENT_ID=hako-local-dev \
HAKO_LOCAL_IDP_SUBJECT=hako-local-user \
build/hako-local-idp
```

Start the Control Plane API with PostgreSQL, loopback issuer, HTTPS, and
owner-only Session signing/HMAC keys:

```sh
HAKO_DATABASE_URL="$HAKO_DATABASE_URL" \
HAKO_COGNITO_ISSUER=http://127.0.0.1:8787/pool \
HAKO_COGNITO_CLIENT_ID=hako-local-dev \
HAKO_API_LISTEN_ADDR=127.0.0.1:8080 \
HAKO_API_TLS_CERT_FILE=/tmp/hako-local-workspace-keys/gateway-cert.pem \
HAKO_API_TLS_KEY_FILE=/tmp/hako-local-workspace-keys/gateway-key.pem \
HAKO_DEFAULT_RESOURCE_PLANE_REGION=local \
HAKO_DEFAULT_WORKSPACE_IMAGE=localhost/hako-workspace-init:local-native \
HAKO_LOCAL_WORKSPACE_SESSION_PRIVATE_KEY_FILE=/tmp/hako-local-workspace-keys/session-private.pem \
HAKO_WORKSPACE_SESSION_KEY_ID=local_session \
HAKO_LOCAL_RESOURCE_PLANE_ID=rp-local \
HAKO_LOCAL_RESOURCE_PLANE_SESSION_KEY_FILE=/tmp/hako-local-workspace-keys/session-hmac.key \
build/hako-api
```

Create a private Firecracker profile from the host kernel and packaged initrd:

```sh
install -d -m 0700 /tmp/hako-local-workspace
python3 scripts/make-local-firecracker-launch-spec.py \
  --kernel /boot/vmlinuz-"$(uname -r)" \
  --initrd /tmp/hako-workspace-initrd.cpio.gz \
  --output /tmp/hako-local-workspace/firecracker-launch.json
```

Start `hako-local-workspace-reconciler` with
`HAKO_LOCAL_FIRECRACKER_STATE_DIR` and `HAKO_LOCAL_WORKSPACE_JOURNAL_DIR`
pointing to private directories under `/tmp/hako-local-workspace`,
`HAKO_LOCAL_FIRECRACKER_BINARY` set to the host's Firecracker v1.18.0 binary,
`HAKO_LOCAL_FIRECRACKER_LAUNCH_SPEC` set to the profile above,
`HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET` set to a mode-0600 socket under a
mode-0700 runtime directory, `HAKO_LOCAL_RESOURCE_PLANE_ID=rp-local`, and
`HAKO_LOCAL_WORKSPACE_GATEWAY_ORIGIN=https://localhost:8443`.

Start `hako-local-workspace-worker` with the same
`HAKO_DATABASE_URL` and Controller socket path. Start the Gateway with
`HAKO_WORKSPACE_RUNTIME_IMPLEMENTATION=firecracker-local`, the same private
state/journal directories and Resource Plane ID, certificate and key paths,
`HAKO_GATEWAY_SESSION_PUBLIC_KEYS_FILE` pointing to `session-public.json`,
both `HAKO_GATEWAY_SESSION_LOOKUP_KEY_FILE` and
`HAKO_GATEWAY_CONNECTION_LEASE_KEY_FILE` pointing to `session-hmac.key`,
`HAKO_GATEWAY_SESSION_LOOKUP_ENDPOINT=https://localhost:8080`,
`HAKO_GATEWAY_CONNECTION_LEASE_ENDPOINT=https://localhost:8080`,
`HAKO_GATEWAY_AGENT_TICKET_KEY_ID=local_agent`,
`HAKO_GATEWAY_AGENT_TICKET_KEY_FILE` pointing to `agent-private.pem`,
`HAKO_GATEWAY_PUBLIC_ORIGIN=https://localhost:8443`,
`HAKO_GATEWAY_ALLOWED_ORIGINS=https://localhost:8443`,
`HAKO_GATEWAY_LISTEN_ADDRESS=127.0.0.1:8443`, and
`HAKO_RESOURCE_PLANE_ID=rp-local`.

The client then uses the existing login and Workspace commands:

```sh
export HAKO_COGNITO_DOMAIN=http://127.0.0.1:8787
export HAKO_COGNITO_CLIENT_ID=hako-local-dev
export HAKO_API_URL=https://localhost:8080
export HAKO_GATEWAY_URL=https://localhost:8443
build/hako login
build/hako create ten_local local-dev
```

The certificate is self-signed; clients must trust it locally for strict HTTPS
verification. For command-line clients, set `SSL_CERT_FILE` to the generated
certificate if it is not installed in the system trust store. The OAuth/JWKS
provider and key generator are covered by `make test-local-idp` and
`make test-local-workspace-keygen`; launch profile validation is covered by
`make test-local-workspace-profile`. The full real-Guest `hako login/create`,
shell, browser IDE, SSH, persistent suspend/resume and delete round trip still
needs to pass as one composed acceptance before Task 98 is complete.

## Local Workspace Outbox worker

`cmd/hako-local-workspace-worker` leases only `operation.requested` events for
the `operation` aggregate. It submits each command to the reconciler's private
Unix socket, applies the standard Resource Controller Result through the
Control Plane's PostgreSQL Operation result store, and acknowledges the Outbox
lease only after the database transaction commits. Failures release the lease
with bounded backoff. Replaying a command after a worker restart is safe because
the operation ID is stable and the local runtime and result projection are
idempotent. Other event families remain available to their normal dispatchers.
Successful results project the provider binding as `firecracker_local`; the
Control Plane accepts that kind for Operation results and Workspace connection
and idle leases, while Lambda MicroVM bindings retain their existing kind.

Run this process alongside `hako-api` and
`hako-local-workspace-reconciler`, using the same local PostgreSQL URL and the
same private socket path:

```sh
HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable' \
HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET="$XDG_RUNTIME_DIR/hako/workspace-controller.sock" \
./build/hako-local-workspace-worker
```

Build and focused tests are available as `make build-local-workspace-worker`
and `make test-local-workspace-worker`. This closes the local Outbox-to-runtime
and result-projection connection. Local API authentication and identity
provider configuration are implemented; the end-to-end authenticated CLI
lifecycle remains open.

The Linux host-local allocation journal is implemented by
`workspacejournal.FileStore`. It stores validated Workspace identities under a
private directory, serializes updates with per-record `flock`, applies the
existing Workspace revision/high-water checks with compare-and-swap, and uses
atomic replacement plus file and directory `fsync`. `ListJournals` provides a
bounded snapshot for controller restart reconciliation. It is single-host
filesystem storage and is not a shared or multi-host coordination service.
Focused journal tests cover controller reopen, stale revisions, concurrent
CAS, context-cancelled lock waits, tombstone listing, and unsafe paths.

`workspaceruntime.LocalMicroVM` implements the Resource Controller runtime
contract on top of that journal. It persists the deterministic allocation ID
before calling an idempotent host driver, pins trusted launch JSON across
resume/retry, and applies the existing revision and delete-tombstone fences.
It requires a process-shared per-Workspace operation lock and holds it through
the provider action and resulting journal checkpoint, so concurrent controllers
cannot interleave suspend/delete or duplicate ensure side effects.
Its bounded `Reconcile` method lists durable allocations, re-reads each under
the same process-shared lock, then reasserts the appropriate driver state. A
running allocation is ensured with its original provider ID and launch payload;
a suspended allocation is confirmed stopped; tombstones retry cleanup. Fake-
driver tests verify ensure/suspend/resume/delete across runtime reconstruction,
retries after a lost driver result, and recovery when a running process was lost
while the journal survived. The local startup reconciler is described below.

`FirecrackerLocalDriver` is the Linux concrete implementation of the driver
contract. It fails startup unless `/dev/kvm` supports the expected KVM API,
uses Firecracker's default seccomp profile, and gives each opaque allocation
ID a private state directory, ext4 Workspace disk, Unix API/vsock sockets,
process identity record, and log. The host dialer opens only the fixed Agent
vsock port; it does not expose a Guest IP or caller-selected port. A trusted launch specification contains canonical
kernel/initrd paths and their SHA-256 pins, bounded vCPU/memory/disk settings,
and kernel boot arguments. The driver verifies those bytes again after
controller reconstruction and refuses to use a replaced image or malformed
ext4 disk. Suspend asks the Guest to shut down with Firecracker's
`SendCtrlAltDel` action. It records quiescence only after the Guest emits the
quiescence marker or Firecracker reports a successful exit from the Guest's
orderly reboot. An API acknowledgment alone is not treated as a clean stop.
Resume starts a fresh VMM and cold boots from the same ext4 Workspace disk.
Delete stops the process before removing only the allocation's files.

The KVM smoke now exercises this concrete lifecycle with a disposable initrd,
checks a file after the same disk is cold-booted, and reconstructs the driver
between suspend and resume. The disposable Guest runs the
`workspaceagent.ListenWorkspaceAgentVsock` listener and the host probes its
HTTP health route before suspend and after cold resume. The actual
`hako-workspace-agent` can optionally serve its existing HTTP/WebSocket handler
on the same fixed Guest port 7000, alongside its inherited Unix listener. The
`HAKO_WORKSPACE_AGENT_VSOCK_ENABLED=true` setting is trusted Guest image
configuration and defaults off. Set the Make variable with the same name to
`true` when building the Workspace image to embed this opt-in; the build
rejects values other than `true` or `false`. HTTP/WebSocket requests still pass
through the same signed Agent-ticket verifier. The local Gateway runtime selects
`firecracker-local` with `HAKO_WORKSPACE_RUNTIME_IMPLEMENTATION`, then reads the
durable FileStore, requires a running allocation on the configured Resource
Plane, signs the same short-lived Agent ticket used by the Lambda path, and
connects only to that allocation's fixed-port vsock listener. The stream
directory derives the state path from the journal's validated Firecracker
allocation ID; callers cannot choose a host path, Guest address, or vsock port.
Shell, exec, file transfer, VS Code, SSH port 22, and authorized port forwarding
share the existing Gateway permission checks and Guest ticket verifier. Local
mode requires clean absolute `HAKO_LOCAL_FIRECRACKER_STATE_DIR` and
`HAKO_LOCAL_WORKSPACE_JOURNAL_DIR` paths, forbids S3 journal configuration, and
does not load AWS credentials unless workload artifacts are separately
configured. Lambda MicroVM mode remains the default and retains its S3 journal
and Region requirements. The local operation API, Outbox worker, authenticated
API/Gateway settings, and process composition are implemented. The driver does
not configure TAP networking or a jailer. The end-to-end authenticated
CLI/Gateway lifecycle remains under acceptance, including shell attach, file
transfer, VS Code, and SSH. The separate quiescence marker path supports a
Guest agent that syncs and unmounts `/workspace` explicitly.

`cmd/hako-local-workspace-guest-init` is the root PID 1 entrypoint for that
local Guest profile. It accepts only the Tenant, Workspace, Resource Plane,
Firecracker allocation ID, and HTTPS Gateway origin in trusted kernel
arguments. It mounts only the attached `/dev/vda` ext4 data disk at
`/workspace`, raises loopback for Guest-local SSH/code-server endpoints, binds
the existing Workspace Agent to those immutable IDs, starts the non-root Agent
with vsock enabled, and stops the Agent, syncs, and unmounts the data disk on a
shutdown signal. It mounts a private devpts instance after devtmpfs and links
`/dev/ptmx` to that instance so PTY master and slave devices use the same
namespace. Its local-disk mount descriptor is
rejected by the Lambda `/run` EFS hook. The workspace image build includes this
binary for ARM64 alongside the Lambda init. The separate local build compiles
the Guest init for the host architecture.

`make build-local-workspace-initrd HAKO_LOCAL_WORKSPACE_ROOTFS_DIR=/absolute/path`
packages an already extracted, host-native Workspace OCI rootfs as a gzip
compressed `newc` initramfs at
`build/local-workspace/workspace-initrd.cpio.gz`. It compiles the Guest PID 1
for the host architecture, requires the Workspace Agent, `sshd`, code-server,
and trusted public keys in the rootfs, checks the `sshd` ELF machine, and
requires Guest PID 1 to be a matching static ELF before packaging. The image's
Lambda entrypoint is replaced by `/init`; the runtime files, libraries, sshd,
code-server, and Agent remain from the OCI rootfs. This target packages a
supplied image; it does not produce the host kernel.
`build-workspace-microvm-image` now accepts `HAKO_WORKSPACE_IMAGE_ARCH=amd64`
or `arm64` and selects an architecture-specific pinned base image. Its default
remains ARM64. On Linux, `make build-local-workspace-native-rootfs` builds the
host-native OCI image, exports its rootfs, and packages the Guest initrd in one
step. The native target requires architecture-matched nerdctl-full,
slirp4netns and code-server artifacts, exact EFS/OpenSSH package versions, and
trusted Agent keys. The x86_64 path no longer needs ARM64 binfmt but cannot use
ARM64 artifacts. The x86_64 OCI image was built from digest-pinned public
artifacts and exact EFS/OpenSSH package pins; its 611 MiB compressed initrd
includes modules required by the running Fedora kernel. An initial KVM run
exposed `EACCES` because the staged initrd root retained mktemp mode 0700. The
packager now sets the root entry to 0755 and a newc-header test enforces that
mode. After rebuilding, the real KVM smoke unpacked the initrd, loaded
vsock/block modules, mounted `/dev/vda` as ext4, enabled loopback, started the
Workspace Agent as UID/GID 100000, and served its identity-bound heartbeat over
host-private vsock. Guest orderly shutdown, ext4 unmount, and allocation
cleanup passed. This smoke uses an 8 GiB disposable Guest to allow unpacking
the approximately 1.7 GiB initramfs. The trusted Agent key in this local image
is a development fixture, not a production key.

For the authenticated local profile, rebuild `cmd/hako-workspace-agent` from
the same checkout and install that binary in the extracted rootfs before
packaging. This keeps the Guest protocol implementation aligned with the Hako
CLI and Gateway binaries. The initrd packager preserves read-only OCI
directories, restores their modes after adding bundled kernel modules, and
cleans its temporary tree without requiring write access to those directories.

Run `make test-local-workspace-initrd` to build a fixture rootfs, inspect the
resulting archive entries, and verify that a Guest PID 1 for another
architecture is rejected. This checks packaging only; it does not validate the
dynamic libraries or services in a real Workspace rootfs.

`FirecrackerLocalWorkspaceLaunchResolver` adds these kernel arguments only to
the host-owned base launch profile: `hako.tenant_id`, `hako.workspace_id`,
`hako.resource_plane_id`, `hako.microvm_id`, and `hako.gateway_origin`. The
base profile cannot predefine `hako.*` identity arguments. The resolver derives
`hako.microvm_id` from the local journal's allocation generation; a new
generation is allowed only after the prior allocation is `terminated` with
cleanup confirmed. The exact launch bytes are then persisted for recovery.

## Local startup reconciliation

`cmd/hako-local-workspace-reconciler` is the host-local Firecracker controller
and recovery process. It checks `/dev/kvm`, opens the private file journal,
reconstructs the `FirecrackerLocalDriver`, exposes the existing
Resource Controller command/result contract on a private Unix socket, and
reconciles immediately at startup and on a bounded interval until
SIGINT/SIGTERM. It emits one redacted JSON line per discovered allocation and
keeps processing when an individual allocation fails. The Unix socket is
created mode `0600` inside an existing directory owned by the current user with
no group/world permission. This restricts operation submission to processes
running under the same local account. Launch kernel/initrd and allocation
state still come only from the private host configuration and journal.

Provide host-owned launch JSON containing pinned kernel/initrd paths and
SHA-256 values, then start it with these settings:

```sh
make build-local-workspace-reconciler
install -d -m 0700 /run/user/1000/hako
HAKO_LOCAL_FIRECRACKER_STATE_DIR=/var/lib/hako/firecracker \
HAKO_LOCAL_WORKSPACE_JOURNAL_DIR=/var/lib/hako/workspace-journal \
HAKO_LOCAL_FIRECRACKER_BINARY=/usr/local/bin/firecracker \
HAKO_LOCAL_FIRECRACKER_LAUNCH_SPEC=/etc/hako/firecracker-launch.json \
HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET=/run/user/1000/hako/workspace-controller.sock \
HAKO_LOCAL_RESOURCE_PLANE_ID=rp_local \
HAKO_LOCAL_WORKSPACE_GATEWAY_ORIGIN=https://workspace.example.test \
build/hako-local-workspace-reconciler
```

Create `/run/user/1000/hako` with mode `0700`, then run
`build/hako-local-workspace-reconciler` as that user and as the owner of both
private state directories. `HAKO_LOCAL_RECONCILE_INTERVAL` defaults to 30
seconds and is bounded to 100ms–10m. `HAKO_LOCAL_RECONCILE_LIMIT` defaults to
the local journal maximum of 100,000 records and can lower the per-pass bound.
A lower limit must still be at least the current number of records because a
truncated listing fails closed instead of silently skipping allocations.

The socket serves `GET /healthz` and `POST /v1/operations` with
`Content-Type: application/json`. The POST body is
the normal versioned Resource Plane `resourcecontroller.Command`; unknown
fields, stale/future commands, wrong Resource Plane IDs and unsupported
operations are rejected by the shared Controller. A successful HTTP response
contains the normal terminal `resourcecontroller.Result`, including the local
Firecracker allocation binding. Runtime errors return a failed result with a
fixed error code and do not expose host paths or Firecracker output. For a
manual local controller smoke, a caller with access to the socket can submit a
command with `curl --unix-socket /run/user/1000/hako/workspace-controller.sock
-H 'Content-Type: application/json' --data-binary @operation.json
http://localhost/v1/operations`; the usual Hako API/CLI deployment and its
Tenant authorization flow are still required for the full `hako create`,
`shell`, suspend/resume and delete lifecycle.

## Host requirements

- Linux with readable and writable `/dev/kvm`.
- A Firecracker binary available as `firecracker` or via `FIRECRACKER`.
- Go, Python 3, `timeout`, `truncate`, `mkfs.ext4`, `find`, and `xz` for the disposable
  Guest-init and ext4-backed disk smoke. `zstd` is needed only when the host's
  `virtio_mmio` module is compressed with zstd.
- A readable host kernel image (x86_64 defaults to
  `/boot/vmlinuz-$(uname -r)`; ARM64 checks `/boot/Image-$(uname -r)` then
  `/boot/Image`), or an explicit `FIRECRACKER_KERNEL` path.
- If `virtio_mmio` is a module for a custom kernel image, provide its matching
  module file with `FIRECRACKER_VIRTIO_MMIO_MODULE`.

Run the host check and a real KVM Guest boot:

```sh
make check-local-workspace-microvm-host FIRECRACKER=/path/to/firecracker
make smoke-local-workspace-microvm-boot FIRECRACKER=/path/to/firecracker
```

The boot smoke selects a static init binary for the host architecture, creates
a temporary initrd, starts Firecracker with its default seccomp configuration,
and checks Guest markers on the serial console. It formats one temporary 64 MiB
raw virtio disk as ext4, mounts it at `/workspace`, and writes a marker file in
one VM. After stopping that VM it boots a second VM with the same disk, mounts
it again, and verifies the file contents. It requires a kernel config with
built-in ext4 (`CONFIG_EXT4_FS=y`); set `FIRECRACKER_KERNEL_CONFIG` alongside a
custom `FIRECRACKER_KERNEL`. This verifies filesystem mounting and persistence
across VM restart, but it has no network interface, Hako Workspace image,
controller, or Gateway components. Temporary files are removed on exit.

Firecracker's production deployment should use its jailer and a purpose-built
Guest kernel/root filesystem. This host smoke does not configure the jailer.
The official [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
describes the kernel/rootfs requirements and the production jailer model.

## Current implementation gap

The local Control Plane/API/CLI, OAuth login, and authenticated
`login/create/wait running/delete/wait deleted` flow pass against a real KVM
Guest. The Guest also passes signed-vsock exec, PTY shell, file upload and
download smoke tests. These channels use a context scoped to the upgraded
WebSocket; using the original HTTP request context caused operations to be
canceled after the upgrade.

The Guest passes signed-vsock exec, PTY shell, upload/download, and same-disk
file persistence after cold resume. Guest shutdown now uses the identity-bound
PID 1 control endpoint on vsock port 7001; PID 1 stops the Agent and unmounts
ext4 before reporting quiescence. The remaining local gap is the complete Hako
CLI interactive shell workflow. Task 97's repository checkout has passed first
install, readback, same-archive retry, and edit-preservation through this real
local Guest flow. The real Gateway
smoke already covers exec, file transfer, shell, Chromium IDE rendering and an
SSH authenticated command over the Firecracker Guest; the controller smoke also
covers restart, replay, same-allocation recovery, and deletion.


`make smoke-local-workspace-controller FIRECRACKER=/path/to/firecracker` builds
the local reconciler, starts it with temporary journal/state/socket directories,
and sends the ordinary Resource Controller commands for Ensure, Suspend,
Resume, and Delete. The real Guest identity heartbeat is checked before Ensure
and Resume are acknowledged, allocation identity must survive cold Resume, and
Delete must remove the Firecracker allocation. The test strips AWS environment
variables and does not contact AWS. The separate full Guest smoke command is
`FIRECRACKER_BIN=/path/to/firecracker FIRECRACKER_KERNEL=/path/to/kernel FIRECRACKER_KERNEL_CONFIG=/path/to/kernel.config FIRECRACKER_VSOCK_MODULE_DIR=/path/to/modules ./scripts/smoke-local-workspace-hako-initrd.sh`.
It boots the packaged Hako Guest, verifies its identity-bound Agent heartbeat
via the fixed host-private vsock endpoint, then deletes the allocation and
requires orderly Guest quiescence. On 2026-11-22 it passed with Firecracker
v1.18.0 after fixing the archived root directory mode.
