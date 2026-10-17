# Task 44: EFS-Backed Workspace Persistence

Status: In progress (local AP provisioner/discovery library only). Priority: P1.

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
cleanup lacks an EFS provisioner. This task is not complete: the guest does not
mount/ack `/workspace`, and Terraform does not create the encrypted filesystem,
private mount targets, or scoped network/IAM rules. Backup/restore, retention,
mount health, orphan cleanup, and live isolation checks remain open. No AWS
changes were made.

## Acceptance criteria

- Tests verify Workspace isolation, stable data across runtime replacement, duplicate create/delete, mount failure rollback, and orphan Access Point discovery.
- No public EFS/NFS ingress is permitted; IAM/network rules are least privilege.
- Document backup/restore, retention, quota, and deletion semantics before enabling production workloads.
