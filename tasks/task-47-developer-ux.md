# Task 47: Developer CLI and Service Catalog

CLI usage output now includes `hako add`, `hako services`, Service usage, and
the `start|stop|remove|rotate-credentials` lifecycle actions. A regression test
checks that these supported Workspace Service commands remain discoverable.

Status: In Progress. `hako shell <tenant-id> <workspace-id>` requests a short-lived RBAC-authorized shell ticket, connects directly to the TLS WSS Gateway, streams terminal input/output, and attempts session revocation on exit. On Linux terminals it uses character mode, sends the initial size, forwards resize signals, and restores terminal settings; framed stream decoding tolerates Gateway fragmentation. The Guest PTY now survives a socket drop within the same MicroVM; the CLI reauthorizes and reattaches with a fresh Hako Session and Agent ticket, while the Guest restricts attachment to the original user and one active stream and buffers up to 1 MiB of output. Cancellation closes the stream cleanly and revokes the session. `hako open <tenant-id> <workspace-id> [local-port]` starts a loopback-only browser tunnel with a dedicated `vscode` Session per TCP stream. The Lambda adapter and Guest Agent bridge only to fixed `127.0.0.1:8081`; the Workspace image starts a pinned ARM64 code-server process as the EFS UID/GID, checks `/healthz`, and stops it with the Agent. It disables code-server auth only because the authenticated Hako Gateway is the sole ingress; no guest TCP port is exposed. `hako ssh-proxy <tenant-id> <workspace-id>` provides an OpenSSH ProxyCommand over a dedicated TLS/WSS permission to fixed private loopback port 22. `hako cp`/`hako cp --from` use a separate signed `file-transfer` Agent route over TLS/WSS; they do not require SSH/SFTP or guest host-key provisioning and restrict paths to `/workspace`. `hako forward <tenant-id> <workspace-id> <remote-port> [local-port]` binds loopback only, issues/revokes one short-lived ticket per TCP connection, and supports up to 16 concurrent local connections. `hako wait` polls an authorized Workspace GET until an observed stable state or timeout and prints state transitions. The versioned PostgreSQL/Redis Service Catalog is wired to authenticated catalog/list/add/action APIs and `hako add`, `hako services`, `hako service start|stop|remove`; `hako services` reports desired/observed revisions and runtime generation, and only presents health from a report matching the active MicroVM binding and current desired revision. Local service reconciliation, health reporting, and cleanup are tested; deployed Lambda MicroVM behavior remains unverified. Tasks 44–46 remain partially open; actual ARM64 image build/MicroVM validation and production runtime routing remain. Priority: P1.

Verification refresh (2026-10-24): `make test-integration-local` passed against
the already-running PostgreSQL, and `make test-software-factory-e2e` passed
with `HAKO_E2E_REQUIRE_PODMAN=1`, confirming the required Podman-backed test
did not skip. These checks verify the local/fake paths, not a deployed Lambda
MicroVM.

## Workspace delete confirmation (2026-10-25)

`hako delete` now requires typing the exact Workspace ID before it sends the
irreversible delete request. Empty input, EOF, a generic `yes`, or a different
ID cancels; non-interactive automation can pass `--yes`. The flag is accepted
only for `delete`. The request remains asynchronous and can be confirmed with
`hako wait ... --state deleted`; `--idempotency-key` remains available for
retries. Unit tests cover parsing, exact confirmation, cancellation, bounded
input, and the non-interactive bypass. See [Workspace CLI](../docs/workspace-cli.md).

## Workspace Service removal confirmation (2026-10-25)

`hako service remove` deletes the Service's scoped data subtree as well as
changing desired state. The CLI now requires typing the exact instance name
before it calls the API; EOF and mismatches cancel. `--yes` is available only
for the `remove` action. Parser and confirmation tests cover both interactive
and automation paths. This is a client-side guard; the authenticated API still
enforces Workspace RBAC and records the lifecycle request.

## Goal

Complete the basic interactive development flow and reproducible local-service templates.

## Work

- Add CLI shell/SSH proxy, browser IDE open, port forward, file transfer, service list/add/remove, and clear lifecycle progress output. (`hako cp` uses its own one-use signed Agent capability and is independent of SSH/SFTP; production Lambda MicroVM image/router validation remains.)
- Preserve current OAuth PKCE/keyring behavior; never request AWS access keys. Use the Gateway Session protocol, not public SSH.
- Define a versioned Service Catalog schema and initial PostgreSQL/Redis templates with pinned images, resources, volume mounts, health checks, and safe default networks. (Catalog contract and built-ins implemented.)
- Add authenticated Workspace-scoped catalog/list/add/action APIs and CLI with Tenant/Workspace visibility checks, idempotent state transitions, conflict handling, tombstones, and audit records. (Implemented locally; runtime execution remains.)
- Keep development DB local to the Workspace Runtime by default; distinguish explicitly persistent managed DBs.
- Document command syntax, idempotency, user-visible errors, and telemetry.

## Progress

- Stabilized the CLI shell Gateway test double so it returns the accumulated
  PTY echo only after receiving terminal EOF. This prevents an artificial
  server close racing the CLI's asynchronous stdin completion and causing an
  unnecessary second Session. The shell unit test and repository-wide Go suite
  pass with this deterministic behavior; production reconnect behavior is
  unchanged.

- Implemented `hako shell <tenant-id> <workspace-id>` over the Session API and
  Gateway WSS protocol. The CLI requires a prior `hako login`, `HAKO_API_URL`,
  and `HAKO_GATEWAY_URL`; see [Workspace CLI](../docs/workspace-cli.md).
- Added `hako wait <tenant-id> <workspace-id> [--state running|suspended|deleted]` for asynchronous Workspace lifecycle requests. It polls the RBAC-protected Workspace GET API, prints only observed-state transitions, defaults to a 10-minute timeout/2-second interval, caps timeout at 30 minutes, and exits on failure, unexpected deletion, API error, or cancellation. Unit tests cover state transitions, target completion, bounds, timeout, cancellation, and API failures. It reports Control Plane observed state only; it does not imply that an undeployed MicroVM executor is active.
- Implemented `hako forward <tenant-id> <workspace-id> <remote-port> [local-port]`.
  The local listener is always `127.0.0.1`; each accepted TCP connection gets a
  separate RBAC-issued ticket and WSS stream, with a 16-connection local cap.
  The Gateway now checks ordinary Workspace ports against a healthy,
  current-generation Service Catalog entry through the signed Resource Plane
  authorization API; Preview remains restricted to its fixed app port. See
  [Task 86](task-86-workspace-service-port-authorization.md).
- Implemented `hako ssh-proxy <tenant-id> <workspace-id>` as an OpenSSH
  ProxyCommand: it requests a dedicated short-lived `ssh-proxy` ticket, relays
  raw bytes over TLS/WSS, and attempts revocation on exit. The Gateway has a
  dedicated route that always dials Workspace-local port 22 and does not
  accept caller-supplied host/port values. TCP/22 is not internet-exposed.
- The Lambda MicroVM Resource Plane adapter now accepts the `ssh-proxy`
  permission only through the Gateway's fixed port-22 route. It forwards the
  one-use capability to Guest port 8080 and the unprivileged Guest Agent then
  dials only `127.0.0.1:22`; arbitrary port values and `port-forward`-only
  tickets are rejected. Local WSS tests cover byte relay, ticket identity,
  and replay rejection. Workspace init now starts image-installed `sshd` on
  loopback with public-key-only authentication and an EFS-backed per-Workspace
  host key; the private key is copied to root-only `/run` for sshd. The
  authenticated `hako cp --from` command can retrieve the public host key.
  ARM64 image build and live MicroVM connection remain unverified; file
  transfer does not depend on this SSH endpoint.
- Added `hako cp` upload and `hako cp --from` download as a dedicated
  `file-transfer` Session and signed, one-use Agent Ticket over TLS/WSS. The
  binary control frame is bounded and validates absolute `/workspace` paths;
  the Linux Agent pins the mounted root, traverses with `openat` and
  `O_NOFOLLOW`, rejects symlinks/non-regular files, caps files at 1 GiB, and
  uploads via fsync plus atomic rename. The CLI downloads through a private
  mode-0600 temporary sibling and commits only after the full stream arrives.
  This removes the sshd/SFTP and host-key dependency; production image and
  Gateway-to-MicroVM routing still need validation.
- Added a local TLS API/Gateway integration test for ticket issuance, shell
  byte streaming, cancellation, authorization denial, TCP forwarding, and
  session revocation, plus configuration and invalid-port tests. Race tests
  pass for the CLI and Gateway packages.
- Added a length-bounded `HAKS` shell frame protocol for input, output, and PTY
  resize. Its incremental decoder handles fragmented/coalesced Gateway byte
  streams. Linux TTY sessions disable echo/canonical buffering, send initial
  dimensions and `SIGWINCH` resizes, and restore the original termios. An
  Agent-to-real-PTY integration test verifies `stty size` after resize.
- Added `hako open` with a loopback-only browser listener and a dedicated
  `vscode` Session per browser TCP connection. The Lambda MicroVM adapter and
  Guest Agent now use a distinct one-use `vscode` ticket and only forward to
  fixed `127.0.0.1:8081`. Local TLS API/Gateway tests cover the permission,
  HTTP byte stream, and Session revocation. The Workspace Agent now starts the
  image-pinned ARM64 code-server after EFS identity binding, verifies its
  `/healthz`, and shuts it down with the Agent. It binds only guest loopback;
  `--auth none` is restricted to the authenticated Gateway tunnel. Image builds
  require an explicit release version and matching archive SHA-256.
- Added a versioned built-in Service Catalog (`postgres`, `redis`) with image
  version policy, bounded resources, private-only TCP ports, safe mount paths,
  fixed exec-form health checks, and generated-secret declarations (never
  secret values). Added authenticated Service Catalog, Workspace Service list,
  and add APIs; Tenant membership and Workspace visibility are checked in the
  read/mutation transaction. Identical additions return the same instance;
  another template cannot take over its name. Creation is audited. CLI commands
  are `hako add` and `hako services`.
- Service endpoints persist desired state and the Resource Plane executor
  reconciles fixed catalog templates, reports health, and confirms cleanup.
  `hako services` exposes revisions and the active runtime generation; its
  health result stays pending until a matching report exists. These paths are
  locally tested but not verified in a deployed Lambda MicroVM. Removal keeps
  a name-reserving tombstone until runtime cleanup is confirmed. Public ingress
  is absent.
- Service desired state writes a revision-fenced Outbox command and routes only through an optional dedicated queue pinned by the Workspace Placement. Task 84 adds the signed Guest endpoint, Catalog reconciler, rootless `nerdctl` engine adapter, root-init durable child-restart fence, HMAC-authenticated revision/binding-fenced health report API, and an SQS consumer/Lambda executable that resolves the durable MicroVM binding and issues port-scoped AWS auth plus one-use Guest capability. PostgreSQL integration, consumer/report unit tests, Resource Plane Terraform mock plans, and dev-root validation cover the local path. The Workspace init image now has caller-pinned `nerdctl-full` inputs and a default-off root-init-supervised rootless containerd opt-in; generated PostgreSQL credential delivery and cross-MicroVM cleanup fencing are also implemented locally. Terraform remains default-off and unapplied; ARM64 image build and real Lambda MicroVM container/network/resource-limit verification remain open.
- The interactive shell transport now reconnects within the same running
  MicroVM. Remaining gates include recovery after Agent/MicroVM replacement,
  actual ARM64 image build/MicroVM validation, and production Gateway routing.

## CLI API wiring refresh (2026-10-24)

- Added an HTTP-backed `hako wait` CLI test using a synthetic local API token.
  It verifies the Tenant/Workspace-scoped GET route, Bearer authorization,
  observed-state output, and fail-closed behavior when the token or API URL is
  missing. Existing polling tests cover transitions, cancellation, API errors,
  terminal states, and timeout. `go test ./cmd/hako -count=1 -coverprofile=...`
  reported 49.1% package statement coverage at that point; this was a
  point-in-time result, not a minimum coverage gate.

## File transfer CLI E2E and coverage refresh (2026-10-25)

Added authenticated local API + TLS/WSS Gateway E2E for `hako cp` upload and
`hako cp --from` download. Both flows verify the file-transfer-only Session,
Tenant/Workspace route, remote `/workspace` path, transfer payload, Session
revocation, and atomic local download commit without leaving a temporary file.
Negative transfer cases verify failed upload reporting and that an incomplete
download preserves the existing destination and removes its temporary file.
`go test ./cmd/hako -count=1 -coverprofile=...` now reports 58.7% package
statement coverage; `workspace_copy.go` is 74.4% overall and its path/ID
validators are 100%. This is local protocol evidence and does not verify a
deployed Lambda MicroVM or Gateway route.

## Port-forward CLI failure-path coverage (2026-10-25)

Added direct `workspaceForward` CLI checks for nil context/output, malformed
remote/local ports, missing login, missing API/Gateway URLs, and a local port
already in use. The existing TLS/WSS test still verifies loopback binding,
per-connection Session authorization, byte relay, and revocation. `go test
./cmd/hako -count=1 -coverprofile=...` passes; `workspaceForward` statement
coverage is now 88.0% and the CLI package is 62.7%. These tests do not verify a
deployed Gateway or Lambda MicroVM.

## SSH proxy CLI failure cleanup (2026-10-25)

Added a direct `hako ssh-proxy` entry-path test where Session issuance succeeds
but the Gateway URL is invalid. The CLI returns an error and revokes the
short-lived Session, proving cleanup on pre-stream connection failure. The
existing TLS/WSS E2E continues to cover the dedicated `ssh-proxy` permission,
byte relay, cancellation, and revocation. `workspaceSSHProxy` entry coverage is
100%; its injected implementation is 75.8% and total `cmd/hako` coverage is
62.9%. This does not prove that the Workspace image has a configured `sshd` or
that production Gateway routing is deployed.

## Acceptance criteria

- CLI unit and authenticated integration tests cover happy path, re-login, authorization failure, disconnect, cancellation, and invalid forwards.
- Service tests cover create/start/health/stop/remove, persistent data behavior, resource limits, and image policy.
- No database/service ports are publicly opened and templates reject unsafe privileged settings by default.
