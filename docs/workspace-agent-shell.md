# Workspace Agent shell and PTY

`internal/workspaceagent` is the interactive Workspace shell runtime. It is
separate from `internal/workloadagent`: CI Workloads retain their fixed,
server-owned command and attempt-scoped tickets, while a developer shell is an
interactive capability authorized by a Workspace session.

## Current local implementation

The Linux PTY manager starts only the image-owned `/bin/sh -i`; callers cannot
select a binary, arguments, environment, or working directory. The root is an
absolute directory supplied by the image/runtime composition and must already
exist. The manager accepts a fixed UID/GID for the shell process. A root guest
init may use this to drop shells to the Workspace EFS Access Point's POSIX
identity; it rejects UID/GID 0. An unprivileged manager may use only its own
identity. The shell starts there with a minimal `HOME`, `PATH`, `TERM`, and
`LANG` environment. Each session receives a cryptographically random 192-bit ID and
has a configurable lifetime (default eight hours, maximum 24 hours). The
manager limits concurrent sessions (default four, maximum 64), terminal
dimensions (500 columns × 500 rows), and individual input frames (64 KiB).
Resizing uses the PTY window-size ioctl. Explicit close, context cancellation,
manager shutdown, and TTL expiry terminate the process group; completed
sessions cannot be reattached.

The manager remains a local runtime primitive, not a public API. The
`workspaceagent.GatewayDialer` adapts it to the authenticated Gateway and
checks that the signed Workspace ID matches the Workspace-local manager before
opening a shell. A local TLS WSS E2E sends a command through that adapter to a
real PTY and verifies the output. This adapter is currently single-Workspace;
the Resource Plane router must select it from verified claims. The adapter
also supports a port stream only to `127.0.0.1:{port}` after the Gateway's
trusted port allowlist authorizes it; it does not accept a destination host.
VS Code is not implemented by this adapter, and shell sessions are not
reconnectable after disconnect. The random PTY session ID is
defense-in-depth, not a substitute for authorization. No public SSH or
additional TCP listener is introduced by this package. The Lambda MicroVM
remains the isolation boundary; this does not claim filesystem confinement
inside the guest.

## Verification

`GOCACHE=/tmp/hako-go-cache go test -race ./internal/workspaceagent ./internal/workspacegateway -count=1`
exercises a real local Linux PTY, shell input/output, resize, environment and
working-directory constraints, input bounds, session limits, completed-session
rejection, explicit shutdown, process-group cancellation, TLS WSS-to-PTY
command/output, and port-forward relay to a loopback listener. These local tests do not establish that the Lambda MicroVM
image has `/dev/ptmx` and `/dev/pts` mounted correctly; that remains part of
the image/runtime PoC.
