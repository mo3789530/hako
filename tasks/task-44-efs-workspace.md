# Task 44: EFS-Backed Workspace Persistence

Status: In progress (AP provisioner, durable runtime binding, Linux mount hook,
and dedicated root init image definition implemented locally; image execution,
per-Workspace IAM/network policy, and recovery/deployment gates remain).
Priority: P1.

## Goal

Keep `/workspace` source data across MicroVM replacement while isolating Tenant/Workspace filesystems.

## Work

- Provision or reuse EFS and create one restricted Access Point per Workspace with stable UID/GID and root path.
- Bind Access Point metadata to Workspace and provider Runtime; mount before reporting Workspace running and unmount before cleanup.
- Make creation/deletion idempotent, compensate Access Point on failed Workspace provisioning, and preserve source data through suspend/resume/replacement.
- Enforce encryption in transit/at rest, security-group scope, backup policy, mount health timeout, and ownership tags.

## Current implementation

`internal/workspacestorage` now creates deterministic, tagged EFS Access Points
with a Workspace-specific root path, non-root POSIX identity, and `0700`
permissions. Retries use the same EFS client token. Deletion checks the exact
filesystem, identity tags, root path, and POSIX settings before calling EFS;
tagged Access Points can be discovered for operator comparison without
automatic adoption or deletion. See [Workspace EFS storage](../docs/workspace-efs-storage.md).

The Lambda MicroVM adapter now persists/reuses the EFS binding, supplies
non-secret mount metadata through the run hook, and removes the AP only after
MicroVM termination. It fails closed when a replacement has no binding or
cleanup lacks an EFS provisioner. A strict Workspace-only `/run` hook handler
and Linux EFS mount executor now validate metadata, run `/bin/mount` with fixed
TLS/IAM/Access Point/region parameters, and acknowledge only after `/workspace`
is seen as mounted. `cmd/hako-workspace-init` and a separate
`images/workspace-init` image wire this hook; the build requires a pinned
`amazon-efs-utils` RPM version. The image build and actual Lambda MicroVM hook
invocation still need verification, and the interactive Agent is not yet
started by the image. The existing Workload Agent image has an incompatible
hook schema. Current Workspaces also share one static execution role, so IAM
authorization does not yet enforce per-Workspace AP isolation.
The root init process now attempts ownership-checked `/workspace` unmount on
graceful shutdown and preserves its marker if the unmount fails. Forced Lambda
MicroVM shutdown behavior remains unverified; controller cleanup still waits
for provider termination before AP deletion.
This task is not complete: Workspace-scoped role and
EFS policies, encrypted EFS/private mount target/scoped network Terraform,
backup/restore, retention, automated orphan cleanup, and live isolation checks
remain open. No AWS changes were made.

## Acceptance criteria

- Tests verify Workspace isolation, stable data across runtime replacement, duplicate create/delete, mount failure rollback, and orphan Access Point discovery.
- No public EFS/NFS ingress is permitted; IAM/network rules are least privilege.
- Document backup/restore, retention, quota, and deletion semantics before enabling production workloads.
