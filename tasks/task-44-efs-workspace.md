# Task 44: EFS-Backed Workspace Persistence

Status: In progress (AP provisioner, durable runtime binding, bounded Linux
mount hook, dedicated root init image, Workspace-scoped IAM role provider,
Lambda runtime role wiring, and opt-in static/dynamic EFS policy modes are
implemented locally. Fake IAM tests cover ensure/drift/delete behavior and
mock plan tests cover role/AP and dynamic path policy. The Resource Plane
Terraform contract now conditionally grants Access Point lifecycle and
Workspace-role operations when dynamic Workspace roles are enabled; role
creation tags, existing-role ownership tags, and Lambda-only PassRole are
separate conditions. Mock plan tests verify the default-off and scoped policy.
Read-only journal/AP reconciliation reports referenced, unbound, stale,
duplicate, mismatched, and missing bindings; it never deletes candidates.
`cmd/hako-workspace-efs-reconcile` lists a bounded, explicitly configured S3
journal prefix, validates and reads each durable Workspace journal, then
performs a read-only paginated EFS comparison with sanitized JSON output. It
retains tombstones until cleanup confirmation and never mutates Access Points.
Real role lifecycle, EFS authorization behavior, private subnet/connector
compatibility, image execution, restore testing, and deployment gates remain
unverified.
Priority: P1.

## Goal

Keep `/workspace` source data across MicroVM replacement while isolating Tenant/Workspace filesystems.

## Work

- Provision or reuse EFS and create one restricted Access Point per Workspace with stable UID/GID and root path.
- Bind Access Point metadata to Workspace and provider Runtime; mount before reporting Workspace running and unmount before cleanup.
- Make creation/deletion idempotent, compensate Access Point on failed Workspace provisioning, and preserve source data through suspend/resume/replacement.
- Enforce encryption in transit/at rest, security-group scope, backup policy, mount health timeout, and ownership tags.

The Linux mount hook now bounds the EFS helper with a 50-second default
timeout and waits for `/workspace` to appear as an NFS mount before
acknowledging the hook. Timeout and helper-success-without-mount cases retain
the ownership marker for an idempotent retry and are covered by local tests.

`internal/workspacestorage` defines a locally tested Workspace execution role
contract: opaque deterministic names, Lambda-only trust, and exact
filesystem/Access Point/TLS-scoped permissions with explicit deny rules. Its
injectable IAM provider creates, validates, and deletes owned roles and rejects
foreign ownership or policy/trust drift. `WorkspaceStorage` composes it with
the AP provisioner. A default-off Resource Plane Terraform policy now grants
the scoped Access Point and execution-role lifecycle actions when dynamic
Workspace roles are explicitly enabled. This is an unapplied IAM contract, not
evidence of live AWS authorization.

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
invocation still need verification. The interactive Agent child is started by
the Workspace init image after the EFS mount and readiness check; the existing
Workload Agent image has a separate hook schema and is not reused for this
purpose.
The root init process now attempts ownership-checked `/workspace` unmount on
graceful shutdown and preserves its marker if the unmount fails. Forced Lambda
MicroVM shutdown behavior remains unverified; controller cleanup still waits
for provider termination before AP deletion.
The Workspace IAM role provider now creates a deterministic `/hako/workspaces/`
role after validating the exact AP; it verifies Lambda-only trust and ownership
tags, attaches one AP/TLS-only identity policy, refuses drift/foreign roles,
and removes the role only after verifying the exact binding. The Resource
Controller Lambda composes the EFS and IAM providers; the Lambda MicroVM runtime
uses and journals that role ARN. The Terraform module exposes a
default-off dynamic resource-policy mode restricted to that role path and
EFS AP ARNs; explicit identity-policy Deny statements prevent cross-AP use.
The Resource Plane Terraform module now defines the default-off lifecycle and
PassRole grants, with mock-plan coverage and dev-root wiring. This remains
local/fake-provider evidence: live IAM authorization, real EFS policy behavior,
restore, orphan reconciliation, and live isolation checks remain open. No AWS
changes were made. The standalone
Terraform module is
[`workspace-storage`](../infra/terraform/modules/workspace-storage); the dev
Resource Plane root can instantiate it only when the optional
`workspace_storage` input is explicitly configured. Its default is `null`.

The storage provisioner now tracks whether an Access Point was created by the
current call. If subsequent IAM execution-role provisioning fails, it removes
only that newly created AP; an AP discovered from a previous attempt is kept
for safety. Fake-provider tests cover both compensation and preservation.
The runtime adapter also has a replacement test proving allocation generation
2 reuses the exact journaled AP/UID/root binding. This verifies metadata
continuity, not actual file persistence through a mounted EFS filesystem.
`go test ./internal/workspacestorage ./internal/workspaceruntime` passes.

The read-only `EFS.Reconcile` primitive compares supplied durable
identity/binding pairs with validated Hako Access Points and returns a stable
report for referenced, unbound, stale, duplicate, mismatched, and missing
entries. It deliberately does not enumerate journals or delete resources;
callers must include terminal tombstones and operators must account for
in-flight provisioning before taking action. See
[`workspace-efs-storage.md`](../docs/workspace-efs-storage.md).

An ARM64 image build was attempted with test-only public keys and the pinned
`amazon-efs-utils` version. Cross-compiled init/agent binaries built, but the
Podman host lacks ARM64 execution emulation (`exec format error` in the AL2023
RPM-install layer); the published MicroVM base image has no amd64 manifest.
The final Workspace image was therefore not produced. Re-run on an ARM64 host
or an explicitly provisioned binfmt/QEMU builder, then verify `/run` on a real
Lambda MicroVM before claiming runtime readiness.

## Acceptance criteria

- Tests verify Workspace isolation, stable data across runtime replacement, duplicate create/delete, mount failure rollback, and orphan Access Point discovery.
- No public EFS/NFS ingress is permitted; IAM/network rules are least privilege.
- Document backup/restore, retention, quota, and deletion semantics before enabling production workloads.
