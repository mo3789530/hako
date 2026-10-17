# Workspace EFS storage

Status: local Access Point provisioning and ownership discovery are implemented
as a library and the Lambda MicroVM adapter provisions/reuses and cleans up the
binding. A dedicated Workspace guest init now handles the `/run` mount contract;
Resource Plane Terraform does not yet create/configure the filesystem. This is
not production-ready and no AWS resources were created.

## Isolation model

Each Tenant/Workspace/Resource Plane identity maps deterministically to one EFS
Access Point on the configured Resource Plane filesystem. The AP pins a stable
non-root POSIX UID/GID, a deterministic root under `/hako/`, and mode `0700`.
The root path includes hashes of the Resource Plane, Tenant, and Workspace IDs;
the AP additionally carries the `hako:managed-by`, Tenant, Workspace, and
Resource Plane ownership tags. The EFS client token is deterministic, so an
ambiguous create can be retried without intentionally creating a second AP.

`internal/workspacestorage` rejects mismatched create responses and validates
all ownership tags, root path, UID/GID, and permissions before deleting an AP.
`DiscoverManaged` lists only the exact configured filesystem and validates
tagged entries; it returns metadata for comparison with durable Workspace
bindings and never adopts or deletes discovered resources.

The runtime journal schema has an immutable `storage_binding` field and
validates its shape across encode/decode. The Lambda MicroVM adapter now
ensures the AP before first allocation, stores the binding with the immutable
launch request, and passes filesystem/AP IDs, region, root path, POSIX IDs,
`/workspace`, TLS, and IAM-auth intent in the non-secret `/run` hook. Replacements reuse the journaled
binding; an old terminated journal without one fails closed rather than
silently starting with empty storage. Delete waits for MicroVM termination,
deletes only the exact owned AP, then confirms the journal tombstone. A failed
cleanup remains retryable.

The guest image still does not implement or acknowledge the mount contract.
The dedicated `images/workspace-init` Workspace image runs
`cmd/hako-workspace-init`, which serves the strict Workspace-specific `/run`
hook as root and acknowledges only after the mount is verified. The image is
separate from the Workload Agent image and does not yet launch the interactive
Workspace Agent; that process must start unprivileged after the mount.
`internal/workspaceagent`'s Linux executor invokes `/bin/mount` without a
shell, with fixed `-t efs` arguments, TLS, IAM authorization, the exact Access
Point, and the Resource Plane region; it writes a root-only idempotency marker before the
side effect and verifies `/workspace` appears as an NFS mount. A pre-existing
unowned mount fails closed. The process must run as root in the guest init
layer, while the interactive Agent should run unprivileged after mount. The
image build requires `HAKO_EFS_UTILS_RPM_VERSION` and pins that package
version. AL2023 publishes the helper in its package repositories ([AWS AL2023
EFS instructions](https://docs.aws.amazon.com/linux/al2023/ug/efs.html)). Build
locally with `make build-workspace-microvm-image`. The image
build has not yet been executed and runtime hook behavior inside an actual
Lambda MicroVM remains unverified. Do not use the existing
`images/workload-agent` for Workspace storage: its lifecycle hook accepts a
different, Workload-specific schema. Therefore the current Runtime adapter
confirms MicroVM `RUNNING`, not that `/workspace` is mounted, and this is not
production-ready. A failed startup before the
runtime journal is created may leave a tagged AP; `DiscoverManaged` exposes it
for reconciliation but no automated orphan deletion exists.

AWS documents Access Point mounts through the EFS mount helper using `tls` and
`accesspoint=` options, and requires TLS when IAM authorization is used. The
helper supports selecting the AWS Region explicitly, which is required here
because a MicroVM guest must not depend on EC2 instance metadata. See [AWS
Access Point mount instructions](https://docs.aws.amazon.com/efs/latest/ug/mounting-access-points.html),
[AWS IAM mount instructions](https://docs.aws.amazon.com/efs/latest/ug/mounting-IAM-option.html),
and the [official EFS mount-helper source](https://github.com/aws/efs-utils).

An Access Point is not, by itself, a Tenant network/IAM boundary. Current
Runtime launch profiles use one static execution role for all Workspaces, so
the new `iam` mount option does not yet distinguish one Workspace from another.
Production must remain disabled until each MicroVM is given a Workspace-scoped
IAM identity/policy restricted to its exact Access Point, the filesystem
policy rejects direct or non-TLS access, and private mount-target security
groups restrict NFS to the reviewed Resource Plane network path. Those policies
and per-Workspace roles are not implemented here; a shared role must not be
treated as sufficient Tenant isolation.

## Mount and network requirements

The Lambda MicroVM `RunMicrovm` request has no EFS mount field. The Workspace
image receives filesystem ID, region, AP ID, root path, and POSIX identity
through the trusted Resource Plane `/run` hook payload, then mounts `/workspace`
before acknowledging that hook. Mount must use the AP and TLS/in-transit
encryption; credentials or secret values must not be placed in the launch
payload. Mounts use the EFS IAM option in addition to TLS; the caller role must
be scoped as described above. A bounded mount timeout and explicit health check
are required.

The EFS mount targets must be private in the Resource Plane VPC. Security
groups must allow NFS/TCP 2049 only from the approved MicroVM connector/security
group; no public NFS ingress is permitted. The AP's POSIX identity and root
restriction are defense in depth, not a substitute for network and IAM policy.
At-rest encryption must be enabled on the filesystem. These infrastructure
controls are not yet provisioned or verified by this repository's Resource
Plane Terraform.

## Backup, restore, quota, and deletion

- Backups and retention are not configured by the current library. Before
  production, select an EFS backup policy and retention window, then document
  restore ownership/permissions and test a restore into a separate filesystem.
- EFS capacity and throughput are shared at filesystem scope. Per-Workspace
  storage quotas are not enforced by Access Points; an AP is not a quota
  boundary. Capacity monitoring and tenant-level policy remain open work.
- Deleting a Workspace must preserve `/workspace` while suspended or while an
  allocation is being replaced. Permanent delete requires an explicit
  Workspace deletion policy, confirmed runtime unmount/termination, backup
  retention decision, and then AP/data deletion. AP deletion alone does not
  erase its directory contents.
- Orphan candidates are discovered by comparing `DiscoverManaged` output with
  durable Workspace/resource bindings. They require operator review; this
  library intentionally does not infer that an unbound AP is safe to delete.

## Local verification

Run `GOCACHE=/tmp/hako-go-cache go test ./internal/workspacestorage ./internal/workspaceagent -count=1`.
Tests cover deterministic Workspace separation, retry identity, malformed AWS
responses, ownership-fenced deletion, absent-AP idempotency, tagged AP
discovery, exact fixed mount arguments, bounded mount-table confirmation,
intent-marker retries, and rejection of an unowned or non-NFS mount. They use
an in-memory EFS API/mount-command fake and do not contact AWS or mount a real
filesystem.

## Remaining Task 44 work

Verify the dedicated Workspace image build, then launch the interactive Agent
unprivileged after the mount. Test suspend/restart/replacement recovery plus
orderly unmount. Add a Resource Plane EFS filesystem, encrypted-at-rest policy, private
mount targets and least-privilege networking/IAM; add mount failure compensation
and orphan reconciliation; and verify backup/restore, retention, and quotas.
Live AWS validation remains subject to explicit approval.
