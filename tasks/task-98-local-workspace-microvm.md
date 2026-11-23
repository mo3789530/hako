# Task 98: Local Workspace MicroVM Runtime

Status: Cloud Hypervisor migration implemented; local KVM acceptance passes for the VMM lifecycle and actual Hako Guest boot/heartbeat. The existing interactive-channel, repository checkout, VS Code, and SSH KVM acceptance was previously run on Firecracker and has not yet been repeated on Cloud Hypervisor. AWS-managed Lambda MicroVM acceptance remains a separate Task 99 gate. Historical Firecracker evidence and earlier implementation notes below are retained as history.

Current Cloud Hypervisor acceptance (2026-11-23): Cloud Hypervisor v54.1 was checksum-verified and booted on this host's KVM API v12. `make smoke-local-workspace-microvm-boot CLOUD_HYPERVISOR=/tmp/cloud-hypervisor-static-v54.1` passed the real VMM lifecycle, fixed-port Agent VSOCK heartbeat, orderly Guest quiescence, cold resume from the same ext4 disk, persistence, and deletion. `make smoke-local-workspace-hako-initrd CLOUD_HYPERVISOR=/tmp/cloud-hypervisor-static-v54.1` passed with the actual Hako Workspace initrd and authenticated Agent identity heartbeat. `make smoke-local-workspace-controller CLOUD_HYPERVISOR=/tmp/cloud-hypervisor-static-v54.1` passed real Controller ensure/suspend/resume, controller restart/retry reuse, and delete against the Hako Guest. Focused Go tests, tagged smoke compilation, launch profile tests, and `go vet` pass. The generated initrd `/init` was rebuilt from current source before this acceptance because the pre-existing local artifact still pinned an `fc_` provider ID. Interactive-channel and signed GitHub checkout acceptance on Cloud Hypervisor remains open. See [Cloud Hypervisor runbook](../docs/local-workspace-cloud-hypervisor.md).

## Local Resource Controller operation endpoint (2026-11-22)

`workspaceruntime.LocalOperationAPI` now accepts the ordinary v2 Resource
Controller command envelope, validates it through `resourcecontroller.Controller`,
executes it against the durable local MicroVM runtime, and returns the standard
terminal Result with its local allocation binding. The startup reconciler serves
the API only on an owner-controlled Unix socket with mode `0600` inside a
private owner-controlled directory. It refuses active, foreign, non-socket, or
insecure-parent paths; stale owned sockets can be replaced. Startup now uses
`FirecrackerLocalWorkspaceLaunchResolver`, binding Guest identity and the
configured HTTPS Gateway origin to the trusted operation/journal generation.
Recovery continues on the same journal. `make test-local-workspace-reconciler`
and `make build-local-workspace-reconciler` pass. The repeatable
`make smoke-local-workspace-controller FIRECRACKER=/path/to/firecracker` also
passes with a real v1.18.0 KVM Guest: Ensure waits for its identity heartbeat,
Suspend observes the quiescence marker, cold Resume keeps the allocation ID
and generation, and Delete removes the VM allocation. The earlier Suspend
timeout exposed that Firecracker API startup preceded Guest PID 1 readiness;
the driver now waits for the fixed-vsock identity heartbeat before Ensure or
Resume reports `running`. The local Outbox worker claims only Workspace
Operation events, forwards them to this socket, applies the terminal result in
PostgreSQL, and acknowledges after commit; unrelated Outbox events remain
untouched. `make test-local-workspace-worker` and
`make build-local-workspace-worker` are the focused checks. API
authentication/identity-provider configuration is implemented and tested.
Remaining local work is composing the services and accepting the normal Hako
CLI/Gateway lifecycle end-to-end. AWS-free Gateway streams and KVM driver
smoke are implemented.

Local identity and launch configuration follow-up (2026-11-22): added a
loopback-only OAuth authorization-code/PKCE and JWKS provider, local API
Session Ed25519 signing and Resource Plane-fenced HMAC resolver configuration,
optional HTTPS serving, an owner-private key bundle generator, and a bounded
Firecracker launch-profile generator. The generated Guest public key is
installed in the extracted host-native rootfs before initrd packaging. Focused
race tests for IdP, key generation, API configuration, worker, and reconciler;
launch-profile tests; the full local stack build; and `git diff --check` pass.
The docs now record the local process profile and certificate trust setup.
Guest command, shell, file-transfer, VS Code bridge and SSH protocol behavior
now pass through signed tickets and host-private vsock on a real KVM Guest.
The real controller smoke now restarts after Resume, replays the exact Resume
command, confirms the same allocation ID, and deletes the Guest. The 2026-11-23
KVM follow-up completed Hako CLI exec/PTTY shell and Task 97 repository
first-install/retry/edit-preservation through the composed Gateway.

## Goal

Run an interactive Hako Workspace on a single Linux host without AWS. Use
Firecracker on KVM for Guest execution and reuse the Hako CLI, Workspace
Agent, Gateway session protocol, and Workspace operation model.

## Work

1. Compose a local deployment with PostgreSQL, the API, a local identity
   provider, and an operation worker using the existing Outbox. Use the normal
   Tenant authorization checks and durable operation acknowledgment.
2. Add a local Resource Controller runtime using Firecracker. Check KVM
   access at startup. Build a host-architecture Guest kernel and root
   filesystem with the Workspace Agent, sshd, and code-server. Attach a
   separate persistent Workspace disk and mount it at `/workspace`.
3. Give every operation a durable allocation record. Serialize concurrent
   operations for a Workspace and reconcile live VMs against those records
   after controller restart. Stop a VM on suspend and boot from the same data
   disk on resume. Record the local lifecycle semantics in the CLI docs.
4. Route Gateway streams to the active Guest through a host-private
   transport. Verify the Workspace and allocation identity and preserve the
   existing shell, SSH, and browser IDE authorization. Keep Guest services on
   private endpoints and explicitly configure Guest outbound access.

## Acceptance criteria

- With no AWS credentials, create a Workspace, use `hako shell`, open the
  browser IDE, and connect from VS Code Remote - SSH.
- Write a file under `/workspace` and verify it survives suspend and resume.
  Retry a create and restart the controller without creating a duplicate VM.
  Delete the Workspace and verify VM and storage cleanup.
- Run the lifecycle smoke on a Linux host with accessible `/dev/kvm`.
  Run operation and authorization tests with a fake VM driver on regular CI.
  Verify Task 97's checkout with the real Guest and persistent disk.

Software Factory Job execution is a follow-up. Its runtime can reuse the
local VM-launch and cleanup mechanics with a separate ephemeral Guest profile.


Guest startup log boundary (2026-11-22): the Guest PID 1 now emits a bounded `failure_class=guest_init` stage instead of formatting the wrapped Agent `execve` error (which had exposed its path and OS details on the serial console). `go test ./cmd/hako-local-workspace-guest-init -count=1`, `go test -race ./cmd/hako-local-workspace-guest-init -count=1`, the Linux amd64 Guest-init build, `make test-local-workspace-initrd`, and `git diff --check` pass. This log-boundary fix was verified at the time. The later non-root Guest execution failure was resolved by the initrd root-mode change documented below; rebuilt full initrd and KVM heartbeat/shutdown smoke now pass.


Non-root Guest execution fix (2026-11-22): the initrd packager copied the rootfs into a private mktemp staging directory and accidentally preserved its 0700 root mode in the archive. UID/GID 100000 could not traverse `/usr`, so all Guest executable starts failed with EACCES. The packager now sets the staged root to 0755; the package test parses the newc `.` header and asserts searchable root mode. The rebuilt native initrd passed `scripts/smoke-local-workspace-hako-initrd.sh` on Firecracker v1.18.0: Guest Agent starts non-root, answers its identity-bound HTTP heartbeat over vsock, and the existing driver completes orderly shutdown and cleanup. `make test-local-workspace-initrd` and `git diff --check` pass. No AWS service was used.


Local reconciler log-boundary follow-up (2026-11-22): allocation reconciliation JSON now emits a fixed failure message instead of raw driver errors, which may contain host paths or configuration details. A regression test supplies a synthetic path/token error and confirms the report retains only trusted allocation identity and the fixed failure class.

Local Outbox worker follow-up (2026-11-22): added `cmd/hako-local-workspace-worker` and `internal/localoperationworker` to claim only Workspace `operation.requested` events, deliver them over the private Controller socket, apply terminal results in PostgreSQL, and acknowledge only after commit. The filtered claim keeps Workload/Check/Service events for their existing dispatchers. Failures use bounded exponential backoff; command/result identity pins are compared before persistence. Focused race tests cover success ordering, failed result persistence, unsupported event rejection, and bounds. Remaining work is local API identity/auth configuration and full service/CLI composition.

The composed result path exposed a provider-kind mismatch: local Firecracker
reports `firecracker_local`, while Control Plane result validation and
Workspace session/idle lease joins had only recognized `lambda_microvm`.
Result validation/projection/deletion now accepts and fences both MicroVM
kinds; connection lease and idle-suspend lookup also recognizes a local
Firecracker binding. A PostgreSQL integration test verifies local binding
creation, transition to the managed binding kind, and cleanup on deletion;
another exercises lease and idle candidate behavior with a local binding.
`go test -race` for operations, result consumer, Workspace activity and the
local worker passes, as do both focused PostgreSQL integration tests.

Validation on 2026-11-22: `make test-local-workspace-worker`,
`make build-local-workspace-worker`, `make test-local-workspace-reconciler`,
`make build-local-workspace-reconciler`, focused `go vet`, and
`git diff --check` pass. PostgreSQL integration test
`TestClaimWorkspaceOperationsLeavesOtherEventFamiliesUnleased` passes against
the existing local `hako-dev-postgres`. The repeatable Firecracker v1.18.0
`make smoke-local-workspace-controller` Ensure/Suspend/cold-Resume/Delete
smoke also passes after this change.


## Composed authenticated KVM smoke (2026-11-23)

The local OAuth/JWKS login and API list/create lifecycle reached a real
Firecracker Guest without AWS. The CLI successfully waited for `running` and
then deleted the Guest and waited for `deleted`; a second clean Guest also
reached `running` using an initrd rebuilt with the private devpts mount and
`/dev/ptmx` symlink. On that Guest, `hako exec ... -- /usr/bin/id` and
`hako exec ... -- /usr/bin/bash -c 'echo hello'` both returned
`Workspace command could not be started`; uploading a small fixture with
`hako cp ... /workspace/smoke.txt` returned `Workspace file upload failed`;
an interactive `hako shell` session connected but showed no prompt or output.
The final test Guest was deleted and allocation cleanup completed.

This narrows the remaining blocker to Guest command/file/PTY behavior after
Gateway authorization and vsock routing. The local end-to-end acceptance still
requires a successful command, shell interaction, file upload/download, browser
IDE and SSH check, file persistence through suspend/resume, controller
restart/retry, and delete cleanup. Do not mark Task 98 complete from the
successful create/running/delete path alone.

## Guest WebSocket channel and cold-resume follow-up (2026-11-23)

The packaged Guest smoke initially returned `exec_failed` for exec and
`upload_cancelled` for file upload. The Workspace Agent was passing the HTTP
request context into long-lived WebSocket handlers. After the WebSocket upgrade,
that request context was canceled while the protocol stream was still active.
Exec, file operations, shell PTY, VS Code, SSH, and port-forward handlers now
use a context scoped to the upgraded WebSocket handler; stream reads/writes
still observe peer closure. A new real-Guest smoke exercises exec (`id` as
UID 100000), PTY command input/output, upload, and download byte equality.
The guest Agent rebuilt into the authenticated initrd passed all four checks
on Firecracker v1.18.0. Focused Go tests and the smoke program build pass.

The first cold-resume attempt exposed an empty SSH host key after the legacy
Ctrl-Alt-Del shutdown path rebooted without running Guest PID 1 teardown. The
host now posts the exact allocation identity to the Guest private shutdown
control endpoint on vsock port 7001. PID 1 validates the bounded request, stops
the Agent, syncs and unmounts ext4, emits its quiescence marker, then reboots.
The real Firecracker v1.18.0 smoke now passes exec/shell/file channels, Chromium
rendering of the VS Code page, and SSH public-key authentication plus `id -u`
execution and Task 97 repository first install/readback/retry/edit preservation
through the real Workspace Gateway and host-private Guest vsock.
Suspend, Driver reconstruction, cold Resume, and pre-existing file byte
comparison also pass. The separate real-controller smoke passes
Ensure/Suspend/Resume, controller restart, exact Resume replay with the same
allocation ID, and Delete. The 2026-11-23 CLI/Gateway follow-up completed the
Hako exec and PTY shell paths plus Task 97 checkout and persistence through this
Guest.

## Real Gateway and controller recovery acceptance (2026-11-23)

The actual Firecracker v1.18.0 Guest now passes the Workspace Gateway stream
path using a signed Gateway Session and the local FileStore-backed vsock
StreamDirectory. Exec, PTY shell, upload/download, Chromium rendering of the
code-server page, and OpenSSH public-key authentication plus a Guest `id -u`
command all pass through Gateway authorization to the Guest. The same checks
pass after same-disk cold resume, including byte comparison of a file created
before suspend. A separate real-controller smoke passes Ensure/Suspend/Resume,
controller process restart, replay of the exact Resume command with the same
allocation ID, and Delete. Focused Guest/Gateway/runtime/worker Go tests and all
49 offline coding-agent Python tests pass. Local acceptance is complete. Configured GitHub App/Broker endpoints and the
deployed AWS runtime remain tracked as external acceptance gates.


Real CLI `exec` via KVM (2026-11-23): added an opt-in integration test, gated by
`firecracker_workspace_smoke`, that creates a real Guest and test API/Gateway,
requests the CLI's normal exec Session, and runs the actual
`workspaceExecWithTimeout` client through TLS Gateway and the signed Guest vsock
stream. UID 100000 output and Session revoke both pass. Run it with:

```sh
HAKO_LOCAL_WORKSPACE_AGENT_PRIVATE_KEY=/path/to/agent-private.pem \
FIRECRACKER=/path/to/firecracker \
FIRECRACKER_KERNEL=/path/to/vmlinux \
FIRECRACKER_INITRD=/path/to/workspace-initrd.cpio.gz \
make test-local-workspace-cli-firecracker
```

The test also verifies Task 97 `install_repository` over signed file-transfer
Sessions: initial install, byte readback via CLI exec, identical retry, then a
Guest-side local edit followed by a rejected archive retry with the edit
preserved. The initial KVM run found `/tmp` was packaged with mode 0755, so UID
100000 could not create the verified temporary archive. The initrd packager now
forces sticky mode 01777 and its newc regression test passes. Rebuilding the
existing extracted rootfs with the current Agent and Guest-init binaries (no
Podman) produced an initrd that passes the full smoke. Standard Go suites and
integration-tag compilation pass; no AWS or GitHub service was contacted.


Local acceptance completed (2026-11-23): the real Firecracker CLI/Gateway smoke
now runs both `exec` and PTY shell, performs Task 97 repository install/readback,
retries identical content, proves a local edit survives a rejected retry, then
cold-suspends/resumes the same allocation and confirms the edited checkout
remains on its persistent disk. The Task 97 result is recorded in
[Task 97](task-97-workspace-github-repository.md). AWS managed-runtime acceptance
remains in [Task 99](task-99-managed-runtime-acceptance.md).
