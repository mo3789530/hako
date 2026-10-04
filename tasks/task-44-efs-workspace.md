# Task 44: EFS-Backed Workspace Persistence

Status: Blocked on Task 43. Priority: P1.

## Goal

Keep `/workspace` source data across MicroVM replacement while isolating Tenant/Workspace filesystems.

## Work

- Provision or reuse EFS and create one restricted Access Point per Workspace with stable UID/GID and root path.
- Bind Access Point metadata to Workspace and provider Runtime; mount before reporting Workspace running and unmount before cleanup.
- Make creation/deletion idempotent, compensate Access Point on failed Workspace provisioning, and preserve source data through suspend/resume/replacement.
- Enforce encryption in transit/at rest, security-group scope, backup policy, mount health timeout, and ownership tags.

## Acceptance criteria

- Tests verify Workspace isolation, stable data across runtime replacement, duplicate create/delete, mount failure rollback, and orphan Access Point discovery.
- No public EFS/NFS ingress is permitted; IAM/network rules are least privilege.
- Document backup/restore, retention, quota, and deletion semantics before enabling production workloads.
