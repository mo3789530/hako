# Local Workspace with Cloud Hypervisor

The Linux host-local Workspace provider uses Cloud Hypervisor on KVM. The
managed AWS Lambda MicroVM provider is a separate implementation and is not
changed by this migration.

## Host requirements

- Linux with readable and writable `/dev/kvm` and KVM API version 12.
- Cloud Hypervisor v54.1 or a compatible release, plus `mkfs.ext4`.
- A readable host kernel with built-in ext4 support and an initramfs containing
  the Workspace Guest. The Guest kernel must enable virtio-vsock.
- A private state directory owned by the controller user.

Run `make check-local-workspace-microvm-host CLOUD_HYPERVISOR=/path/to/cloud-hypervisor`
to check the executable and KVM API. `make smoke-local-workspace-microvm-boot`
boots a disposable Guest, verifies ext4 `/workspace`, probes the fixed Agent
vsock endpoint, orderly-suspends, cold-boots from the same disk, and verifies
persistence. Set `CLOUD_HYPERVISOR_KERNEL` and
`CLOUD_HYPERVISOR_INITRD` to select the Guest kernel and initramfs.

Build the local processes with `make build-local-workspace-stack`. Configure
the reconciler with `HAKO_LOCAL_CLOUD_HYPERVISOR_BINARY`,
`HAKO_LOCAL_CLOUD_HYPERVISOR_STATE_DIR`,
`HAKO_LOCAL_CLOUD_HYPERVISOR_LAUNCH_SPEC`,
`HAKO_LOCAL_WORKSPACE_JOURNAL_DIR`, `HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET`,
`HAKO_LOCAL_RESOURCE_PLANE_ID`, and `HAKO_LOCAL_WORKSPACE_GATEWAY_ORIGIN`.
Configure the Gateway with
`HAKO_WORKSPACE_RUNTIME_IMPLEMENTATION=cloud-hypervisor-local`,
`HAKO_LOCAL_CLOUD_HYPERVISOR_STATE_DIR`, and
`HAKO_LOCAL_WORKSPACE_JOURNAL_DIR`; its existing TLS, session verification,
key, lookup, and lease settings remain required. Generate the private base
launch profile with `scripts/make-local-cloud-hypervisor-launch-spec.py` and
the selected kernel and initrd.

## Runtime behavior

The durable Workspace journal and ext4 data disk are independent of a VMM
process. Each allocation launches Cloud Hypervisor with a private Unix API
socket, a fixed host-private VSOCK socket, a virtio disk, and pinned kernel and
initramfs digests. The host invokes the Cloud Hypervisor CLI with boot
configuration; it then uses the HTTP API to inspect VM state and request
orderly power-off. Reconciliation retains generation fencing and refuses to
replace a VM until the Guest confirms filesystem quiescence.

The host connects to the VSOCK Unix socket and sends `CONNECT <port>\n`. The
tested Cloud Hypervisor v54.1 host interface then returns `OK <host-port>\n`;
the host consumes only that line, preserving any Guest application bytes that
follow it. The Workspace Agent then speaks its existing authenticated
HTTP/WebSocket protocol on the stream. Cloud Hypervisor's published guide
documents the CONNECT-prefixed stream flow and separately calls out the ACK
for SEQPACKET, so keep this handshake covered by the real version-pinned smoke.
The Agent and control ports are fixed at 7000 and 7001; callers cannot choose
arbitrary Guest ports through this dialer.

## Firecracker state transition

This implementation changes the local provider binding from
`firecracker_local` to `cloud_hypervisor_local`, and changes local allocation
IDs from `fc_` to `ch_`. It does not attach to or convert a live Firecracker
process. Before switching an existing host, stop new local allocations and
shut down the old reconciler; preserve or export each Workspace disk before
removing the old allocation state. Recreate/rebind Workspaces through the
Cloud Hypervisor provider. Managed Lambda MicroVM bindings are unaffected.

## Verification status

Unit and package tests cover launch argument validation, private socket paths,
fixed-port VSOCK routing, operation reconciliation, and resource binding. The
real-KVM smoke is the acceptance gate for process lifecycle, Agent heartbeat,
orderly shutdown, cold resume, disk persistence, and cleanup. Passing this local
smoke does not validate the separately managed AWS Lambda MicroVM path or its
EFS/network behavior.

The previous [`local-workspace-microvm.md`](local-workspace-microvm.md) and
Task 98 entries retain historical Firecracker results. Those results do not
constitute Cloud Hypervisor acceptance.
