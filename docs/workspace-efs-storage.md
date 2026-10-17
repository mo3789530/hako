# Workspace EFS storage

Status: local Access Point provisioning and ownership discovery are implemented
as a library. The Workspace controller does not yet call this library, the
MicroVM guest does not mount it, and Resource Plane Terraform does not create
or configure the filesystem. This is not production-ready and no AWS resources
were created.

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

The Resource Plane runtime must persist the returned binding before launching
the MicroVM. On teardown it must first stop the allocation, confirm guest
unmount/termination, durably record cleanup progress, and only then delete the
AP. The library does not yet provide that orchestration or persist the binding
in the Workspace runtime journal. A failed startup must retain enough durable
state to retry cleanup; it must not delete an AP while an ambiguous MicroVM may
still be using it.

## Mount and network requirements

The Lambda MicroVM `RunMicrovm` request has no EFS mount field. A future guest
bootstrap contract must receive the filesystem ID, AP ID, and mount target
through trusted Resource Plane configuration, then mount `/workspace` before
the controller reports `running`. Mount must use the AP and TLS/in-transit
encryption; credentials or secret values must not be placed in the launch
payload. A bounded mount timeout and explicit health check are required.

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

Run `GOCACHE=/tmp/hako-go-cache go test ./internal/workspacestorage -count=1`.
Tests cover deterministic Workspace separation, retry identity, malformed AWS
responses, ownership-fenced deletion, absent-AP idempotency, and tagged AP
discovery. They use an in-memory EFS API fake and do not contact AWS.

## Remaining Task 44 work

Integrate the provisioner with the durable Workspace journal and lifecycle
controller; implement guest mounting and unmount confirmation; add a Resource
Plane EFS filesystem, encrypted-at-rest policy, private mount targets and
least-privilege networking/IAM; add mount failure compensation and orphan
reconciliation; and verify backup/restore, retention, and quotas. Live AWS
validation remains subject to explicit approval.
