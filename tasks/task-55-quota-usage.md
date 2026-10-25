# Task 55: Workload/Service Quotas and Usage UX

Status: In Progress (Workspace aggregate quota read API and `hako usage` are
implemented and PostgreSQL integration coverage verifies membership isolation,
pending-delete reservations, confirmed release, and the fixed limit of 10.
Concurrent Workload admission reserves one of five Tenant-wide slots in the
creation transaction, returns a stable 409 on exhaustion, preserves idempotent
replay, releases the slot on terminal transition, and backfills active runs
during migration. PostgreSQL concurrency tests cover the exact cap and reuse.
Tenant configuration also has a queued Workload backlog cap (default/platform
ceiling 5, constrained to the concurrent Run limit). `pending`/`queued` runs
reserve the queue budget in the same OCC-guarded creation transaction;
dispatch to `provisioning` releases that queue capacity while keeping the
concurrent Run reservation. Owner/Admin API and CLI configuration and usage
reporting are included, with queue-exhaustion and release integration tests.
Workspace-local Service admission now enforces aggregate 2 GiB/1500 millicore
catalog-request limits with a durable reservation table and per-Workspace OCC
guard row (Aurora DSQL-compatible; no row locks), retains reservations during
pending stop/delete, and returns a stable 409 on exhaustion. PostgreSQL API
integration covers the exact boundary. Workspace-scoped authenticated API and
`hako service usage` report catalog-request reservation totals while explicitly
not presenting them as measured usage. Tenant Owners/Admins can configure lower
Workspace and concurrent Workload limits up to platform ceilings, with changes
below current usage rejected and admission serialized under Aurora DSQL OCC.
Tenant artifact quota is now configurable from 1 byte through 100 GiB (10 GiB
default). New Workload admission reserves 100 MiB for its bounded redacted log
artifact in the same Tenant OCC budget; artifact registration converts it to
retained bytes and terminal runs release unused reservation. Usage reports
stored and reserved bytes separately; Owner/Admin API and CLI configure it.
Workspace Service admission caps are also configurable per Tenant down from the
2 GiB/1500mCPU platform ceiling. Service start/admission and confirmed release
share the Tenant OCC guard; quota changes are rejected if any Workspace's
current Service reservation exceeds the proposed limits. Usage reports the
effective limits. These are still requested-resource budgets, not measured
Runtime enforcement. The store provides a bounded expired-artifact listing and
exact-reference/expiry-conditional metadata deletion. A Resource Plane cleanup
core validates a configured bucket/prefix, deletes each S3 object before
requesting metadata release, bounds batches, and isolates per-object failures;
it does not connect to Control Plane DSQL. The Control Plane now has
Resource-Plane-scoped HMAC candidate/acknowledgement endpoints and a signed
client which binds the query and exact artifact identity. Resource Plane S3
The Control Plane API has a dedicated per-Plane cleanup HMAC resolver and
Terraform Secret-ARN map, separate from execution metadata keys. The Resource
Plane Workload Controller has a scheduled cleanup event path;
Terraform keeps it default-off, scopes `s3:DeleteObject` to the configured
Artifact prefix, and adds a separate schedule/alarm. Local PostgreSQL E2E
verifies cross-Plane candidate isolation, signed requests,
object-delete-before-metadata-release, and idempotent acknowledgement. No AWS
deployment or Secret retrieval has been performed; cross-account Secret policy
and bucket versioning/lifecycle posture must be reviewed because the object
bucket is not owned by this module. Tenant-configurable Preview concurrency
(default 2, ceiling 5) and lifetime (5 minutes–24 hours), same-Tenant OCC
admission, API/CLI configuration, and usage reporting are implemented.
The Preview concurrency budget is shared by PR Preview webhook admission and
Workload Preview Runs. Cleanup-pending PRs retain their slot until cleanup is
confirmed, quota-rejected PRs reserve no capacity, and `hako usage` reports
the combined count. PostgreSQL integration also verifies that lowering the
Preview count below active PR reservations or lowering lifetime below a
recorded active PR expiry is rejected without changing the configured limits.
PostgreSQL lifecycle coverage exercises
cancel-before-start, failed provisioning, timeout expiry, duplicate terminal
transitions, release of both Workload and Artifact reservations, and recovery
of stale terminal reservations during admission. `hako tenant quota set` now
fetches current limits and usage before mutation, shows current reservations
and requested limits, and requires explicit `y`/`yes` confirmation. `--yes` is
required for non-interactive automation; EOF and any other answer cancel
without calling the update API. Parser, defaults-preservation, confirmation,
rejection, and fail-closed EOF behavior have CLI unit coverage. Measured live
resource usage remains future work; the CLI continues to label these values
as reservations/configured limits and does not imply billing.
Priority: P3.

The queued-Workload budget and Tenant aggregate Workspace compute reservation
portions are implemented and locally verified. Tenant Owners/Admins can set
optional profile-based CPU and memory reservation ceilings. Workspace create
admission and quota updates check active reservation snapshots under the shared
Tenant OCC guard; active Workspaces without a Runtime Class profile fail closed
when a compute ceiling is enabled. Usage reports missing profiles separately.
These budgets are estimates, not measured runtime usage; billing remains future
work.

## Goal

Protect Tenant and platform capacity and make expected resource use visible before a Job, Service, or Workspace starts.

## Work

- Define limits for concurrent Workload Runs, profile-based aggregate Workspace compute CPU/memory, queue backlog, service memory/CPU, artifact bytes, and preview lifetime.
- Add transactional admission/reservation and release-on-terminal/cleanup semantics; do not trust user-reported actual usage as authoritative billing data.
- Add Tenant Owner/Admin APIs for quota configuration and read-only usage views with pagination.
- Show validation, current usage, estimated Runtime class, and safe confirmation in CLI without exposing other Tenant usage.

Usage reports active Workspace reservations (limit 10) and concurrent
Workload Runs (limit 5), not measured resource consumption. Service requests
are capped per Workspace by Tenant-configurable limits up to 2 GiB and 1500
millicores; this is an admission budget, not an observed runtime limit. Workspace usage
uses the same active-state rule as admission, including pending deletion.
Workload usage includes pending, queued, provisioning, running, and
cancel-requested Runs until a terminal result is durably committed. Members
must have membership in the requested Tenant; foreign-Tenant reads are hidden
as not found, and no Workspace names or billing data are returned.
`hako usage <tenant-id>` prints both `used/limit` values.
`hako service usage <tenant-id> <workspace-id>` reports durable per-Workspace
Service reservation totals and limits; accepted stopped/deleted reports release
exactly one reservation, and Members can only read visible Workspaces.
Tenant Owners/Admins can read/update Tenant Workspace and Workload limits;
updates cannot exceed platform ceilings or lower a limit below current
reservations. Admission and configuration write the same Tenant quota revision
row to serialize under Aurora DSQL OCC. See [Tenant quota configuration](../docs/tenant-quotas.md).

## Acceptance criteria

- Concurrent integration tests prove limits cannot be exceeded under OCC/transaction retries and reservations release exactly once.
- Tests cover canceled-before-start, failed provisioning, timeout, orphan reservation cleanup, and duplicate terminal events.
- Tenant members cannot read or modify other Tenant quotas/usage; sensitive billing identifiers are not exposed.
- Document configured quota versus measured usage and explicitly defer billing until a separate ADR.

## Local integration follow-up

The Resource Plane expired-artifact candidate query now returns the Run
correlation ID required by the shared artifact response projection. The
signed candidate-listing and cleanup acknowledgement PostgreSQL E2E covers
the endpoint and cross-Plane isolation.

## Quota and usage CLI/API wiring refresh (2026-10-24)

- Added local HTTP-backed CLI coverage for Tenant usage, quota GET/PUT, and
  Workspace Service reservation usage. Tests verify the authenticated
  Tenant-scoped paths, Bearer token, used/reserved values, quota revision, and
  preservation of omitted quota settings when only Workspace/Workload limits
  are changed. Missing token/API URL fail closed. Values remain configured
  admission reservations; this does not claim live resource measurement.
  The focused CLI package now reports 55.7% statement coverage.

## Quota confirmation I/O failure coverage (2026-10-25)

The quota update path now has regression coverage for a terminal that cannot
display the confirmation prompt and for an input stream that fails while
reading the answer. Both failures abort before the mutation callback; only an
explicit confirmation or the existing `--yes` path can update the Tenant
limits. This supplements the existing decline/EOF checks and does not change
the quota API or its configured-reservation semantics.
