# Workspace EFS storage

Status: local Access Point provisioning and ownership discovery are implemented
as a library and the Lambda MicroVM adapter provisions/reuses and cleans up the
binding. A dedicated Workspace guest init handles the `/run` mount contract.
The dev Resource Plane Terraform root now optionally wires the encrypted EFS
module, default-off, with exact role-to-Access-Point policy bindings. This is
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

`EFS.Reconcile` accepts the Resource Plane's durable journal identity/binding
pairs and returns a stable, read-only report: `referenced`, `unbound_candidate`,
`stale_candidate`, `duplicate_identity`, `identity_mismatch`, or
`missing_access_point`. The caller must include terminal journal tombstones
until cleanup is confirmed. An unbound AP can be a create whose journal write
has not completed, so candidate output is not deletion authorization; check
in-flight operations and the retention policy before taking action.
`go run ./cmd/hako-workspace-efs-reconcile --region ap-northeast-1 --filesystem fs-0123456789abcdef0 --journal-bucket hako-rp-journal --journal-prefix workspace-journal/rp-tokyo-01`
connects the primitive to the configured Resource Plane S3 journal and EFS.
It lists only the explicitly configured journal prefix, fetches and validates
each journal, and includes storage bindings until deletion cleanup is confirmed.
The command uses the ambient AWS SDK credential chain for S3
`ListObjectsV2`/`GetObject` and EFS `DescribeAccessPoints`, has a 60-second
deadline, caps listing at 100,000 journals, and emits a deterministic JSON
report marked `read_only: true`. It requires no Secret Manager access and has no
create/delete call path. Errors are sanitized; candidates remain advisory and
are never adopted or deleted.

The runtime journal schema has an immutable `storage_binding` field and
validates its shape across encode/decode. The Lambda MicroVM adapter now
ensures the AP before first allocation, stores the binding with the immutable
launch request, and passes filesystem/AP IDs, region, root path, POSIX IDs,
`/workspace`, TLS, and IAM-auth intent in the non-secret `/run` hook. Replacements reuse the journaled
binding; an old terminated journal without one fails closed rather than
silently starting with empty storage. Delete waits for MicroVM termination,
deletes only the exact owned AP, then confirms the journal tombstone. A failed
cleanup remains retryable. A local runtime test confirms a replacement
allocation reuses the exact journaled AP, POSIX identity, and root path; actual
file persistence through Lambda MicroVM mount/replacement still needs an
ARM64 image and live runtime validation.

The hook also carries Tenant, Workspace, and Resource Plane IDs as non-secret
identity metadata. `NewWorkspaceMountLifecycleHandler` can bind a
`WorkspaceRuntimeBinder` after the mount succeeds and before returning HTTP 200;
the Workspace init image composes this mount hook with the unprivileged
interactive Agent and pinned code-server child. The Agent binds those IDs to
the provider MicroVM identity before serving Workspace sessions.

The dedicated `images/workspace-init` Workspace image runs
`cmd/hako-workspace-init`, which serves the strict Workspace-specific `/run`
hook as root and acknowledges only after the mount is verified. The image is
separate from the Workload Agent image. It launches the interactive Workspace
Agent and pinned code-server unprivileged after the mount and Workspace
identity have been bound.
`internal/workspaceagent`'s Linux executor invokes `/bin/mount` without a
shell, with fixed `-t efs` arguments, TLS, IAM authorization, the exact Access
Point, and the Resource Plane region; it writes a root-only idempotency marker before the
side effect, bounds the EFS helper to 50 seconds by default, and waits for
`/workspace` to appear as an NFS mount before acknowledging the run hook. A
helper timeout or successful helper exit without a healthy mount fails closed
and retains the marker for retry. A pre-existing
unowned mount fails closed. The process must run as root in the guest init
layer, while the interactive Agent should run unprivileged after mount. The
image build requires `HAKO_EFS_UTILS_RPM_VERSION`,
`HAKO_CODE_SERVER_VERSION`, `HAKO_CODE_SERVER_SHA256`, and public Agent keys;
package/archive versions and the code-server digest are pinned build inputs.
AL2023 publishes the helper in its package repositories ([AWS AL2023 EFS
instructions](https://docs.aws.amazon.com/linux/al2023/ug/efs.html)). Build
locally with `make build-workspace-microvm-image`. The image
build has not yet been executed and runtime hook behavior inside an actual
Lambda MicroVM remains unverified. AWS documents that `RUNNING` is reached
only after the `/run` hook completes; this hook returns success only after the
mount health check. The provider's documented state contract therefore fences
Workspace-ready reporting behind mount completion, but the image/hook/state
integration has not been verified in a real MicroVM. Configure the image's
`runTimeoutInSeconds` to at most 60 seconds (AWS maximum); the local mount
helper uses a 50-second deadline to leave response margin. Do not use the existing
`images/workload-agent` for Workspace storage: its lifecycle hook accepts a
different, Workload-specific schema. Therefore the current Runtime adapter
confirms MicroVM `RUNNING`; by the documented lifecycle this is after `/run`
completion, but real mount-to-state behavior remains unverified and this is not
production-ready. A failed startup before the
runtime journal is created may leave a tagged AP; `DiscoverManaged` exposes it
for reconciliation but no automated orphan deletion exists.

On graceful init-process shutdown, the root executor attempts to unmount only
when its root-owned marker matches a managed mount; successful unmount is
verified before removing the marker. Failed/busy unmount preserves the marker
for diagnosis. Lambda MicroVM termination delivery and forced-shutdown behavior
have not been verified, so controller-side deletion still waits for runtime
termination before deleting the Access Point and must not rely on guest
unmount as the only cleanup guarantee.

AWS documents Access Point mounts through the EFS mount helper using `tls` and
`accesspoint=` options, and requires TLS when IAM authorization is used. The
helper supports selecting the AWS Region explicitly, which is required here
because a MicroVM guest must not depend on EC2 instance metadata. See [AWS
Access Point mount instructions](https://docs.aws.amazon.com/efs/latest/ug/mounting-access-points.html),
[AWS IAM mount instructions](https://docs.aws.amazon.com/efs/latest/ug/mounting-IAM-option.html),
and the [official EFS mount-helper source](https://github.com/aws/efs-utils).

An Access Point is not, by itself, a Tenant network/IAM boundary. Hako now has
a locally implemented Workspace-scoped execution-role provider and the Lambda
MicroVM runtime pins its role ARN with the EFS binding. Dynamic role and EFS
policy provisioning remain default-off and have only fake-provider/mock-plan
coverage. Production must remain disabled until live IAM authorization,
filesystem policy, private mount-target security groups, and Lambda MicroVM
connector compatibility are reviewed. A shared role must not be treated as
sufficient Tenant isolation.

`internal/workspacestorage` now provisions an opaque deterministic execution
role through an injectable IAM API. It verifies the exact path, ownership
tags, Lambda MicroVM trust, and sole inline EFS policy; refuses foreign roles
or drift; and performs ownership-checked cleanup. The Resource Plane runtime
uses and journals that role ARN. The identity policy permits
`ClientMount`/`ClientWrite` only on the exact filesystem through the exact
Access Point with TLS required; explicit denies block root, non-TLS, and other
Access Points. Fake IAM tests cover ensure/retry/drift/compensation/delete.
The default-off Terraform dynamic mode permits the generated role path through
EFS Access Points over TLS. No real IAM calls were made and the EFS policy has
not been applied. The Resource Plane Terraform module now has a separate
default-off `workspace_storage_runtime` IAM contract. The dev root supplies it
only when dynamic Workspace roles are enabled and passes the exact EFS ID/ARN
from the storage module. Access Point lifecycle is constrained to that
filesystem and Hako/Resource Plane tags. Role creation uses request-tag
conditions; operations on existing roles use resource-tag conditions;
PassRole is limited to `/hako/workspaces/*` and Lambda. The policy does not
grant trust-policy updates or managed-policy attachment. Local fake IAM and
Terraform mock-plan tests cover this contract, but production remains disabled
until the live EFS policy and Resource Controller IAM lifecycle are reviewed.
The explicit identity-policy deny for any other Access Point is intentional:
same-account identity and resource policies can contribute allows by union,
while an explicit deny overrides either policy's allow. A future dynamic EFS
resource policy must preserve that deny boundary rather than treating an
identity policy's missing allow as a restriction.

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
At-rest encryption must be enabled on the filesystem. An opt-in Terraform
module describes encrypted EFS, exact role-to-Access-Point IAM/TLS resource
policy statements, private mount targets, and connector-SG-scoped NFS. The dev
Resource Plane root exposes it through the nullable `workspace_storage` object;
the default `null` creates no storage resources. Static role/AP bindings must
be unique and exact. Explicit Deny statements reject unconfigured principals
and prevent a configured role from using another AP, even if an identity
policy is broader. The Resource Controller composes its EFS Access Point
provisioner with a deterministic, ownership-checked Workspace IAM role
provider, and the Lambda MicroVM runtime journals and uses that per-Workspace
role. Dynamic resource-policy mode is opt-in and restricted to the generated
role path and same-account/Region APs; each generated role permits only its
own AP.

The IAM contract is now represented in Terraform and enabled only together
with explicit dynamic Workspace-role mode; it remains unapplied. Caller-
provided subnet privacy, Lambda MicroVM connector compatibility, EFS Access
Point isolation, and live mount behavior are not verified. Keep dynamic mode
disabled until the IAM policy, networking, and live behavior are reviewed. See the
[`workspace-storage` Terraform module](../infra/terraform/modules/workspace-storage/README.md).

## Backup, restore, quota, and deletion

- The Terraform module configures daily AWS Backup recovery points with a
  35-day default retention window. Before production, validate restore
  ownership/permissions and test a restore into a separate filesystem.
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
discovery and journal-binding reconciliation, exact fixed mount arguments,
bounded mount-table confirmation,
intent-marker retries, ownership-checked unmount/retry, and rejection of an
unowned or non-NFS mount. They use
an in-memory EFS API/mount-command fake and do not contact AWS or mount a real
filesystem.

## Remaining Task 44 work

The Workspace image now includes the distinct interactive Agent child; root
init starts it with the Access Point UID/GID only after mounting EFS, waits for
readiness, and terminates it before ownership-checked unmount. The provisioner
compensates a newly created Access Point if execution-role provisioning fails,
and the runtime test verifies that a replacement allocation reuses the exact
journaled AP/UID/root binding. These tests do not verify file contents on a real
mounted filesystem. The image build requires a public verification-key JSON
file. An ARM64 Podman build was attempted: cross-compilation succeeded, but the
host cannot execute the ARM64 AL2023 RPM-install layer, and the MicroVM base
image has no amd64 manifest. No final image was produced; use an ARM64 or
explicitly configured binfmt/QEMU builder. The real Lambda `/run` hook remains
unverified. Live EFS IAM policy and mount validation, orderly/forced shutdown
behavior, orphan reconciliation, and backup/restore, retention, and quota
verification remain open.
Live AWS validation remains subject to explicit approval.

Authoritative references: [EFS Access Point IAM condition key](https://docs.aws.amazon.com/efs/latest/ug/access-points-iam-policy.html), [EFS resource policy example](https://docs.aws.amazon.com/efs/latest/ug/security_iam_resource-based-policy-examples.html), [AWS Backup for EFS](https://docs.aws.amazon.com/efs/latest/ug/awsbackup.html), and [EFS lifecycle management](https://docs.aws.amazon.com/efs/latest/ug/enable-lifecycle-management.html).

Lambda MicroVM documents that the `/run` hook completes before the MicroVM
enters `RUNNING`, and that hook timeouts are capped at 60 seconds. See [MicroVM
runtime lifecycle](https://docs.aws.amazon.com/lambda/latest/dg/microvms-how-it-works.html)
and [MicroVM hook configuration](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_MicrovmHooks.html).
See also [IAM identity/resource policy evaluation](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_evaluation-logic.html).
