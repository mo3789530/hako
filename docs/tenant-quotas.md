# Tenant quota configuration

Tenant Owners and Admins can read and configure admission limits:

```http
GET /v1/tenants/{tenant_id}/quotas
PUT /v1/tenants/{tenant_id}/quotas
Content-Type: application/json

{"workspaces_limit": 6, "workload_runs_limit": 3, "queued_workload_runs_limit": 2, "preview_runs_limit": 2, "preview_lifetime_seconds_limit": 86400, "artifact_bytes_limit": 2147483648, "service_memory_bytes_limit": 1073741824, "service_milli_cpu_limit": 750, "workspace_compute_milli_cpu_limit": 4000, "workspace_compute_memory_mib_limit": 8192}
```

Defaults and platform ceilings are 10 active Workspaces, 5 concurrent
Workload Runs, at most 5 pending/queued Workload Runs, 2 concurrent Preview Runs (maximum 5), a Preview lifetime of
24 hours, and 10 GiB / 100 GiB of retained Artifact bytes. Tenant
configuration may lower these values but cannot raise them above the platform
ceiling. The queued Workload limit cannot exceed the concurrent Run limit and
counts Runs in `pending` or `queued`; dispatch to `provisioning` releases a
queue slot while retaining the concurrent Run reservation. Per-Workspace
aggregate Service request limits default to 2 GiB and
1500 millicores and can be lowered per Tenant; the fixed platform ceilings
remain 2 GiB/1500 millicores until Runtime Class enforcement is verified.
Tenant Workspace compute budgets can additionally cap the sum of active
Workspace Runtime Class reservation profiles at 80,000 millicores and 163,840
MiB. A value of zero disables that Tenant aggregate cap. These are profile
reservation weights from `runtime_class_resources`, not measured or guaranteed
Lambda CPU/RAM, and are separate from per-Plane capacity admission. When a
Tenant enables either cap, every active Workspace must have a CPU and memory
profile; missing profiles fail closed. Workspace creation reserves the profile
weights transactionally under the Tenant quota OCC guard, while confirmed
Workspace deletion releases them. Lowering a cap below existing reservations
returns HTTP 409. Usage exposes the aggregate reservation and the number of
active Workspaces with missing profiles.
Artifact and Service limits are specified in bytes/millicores in the API. A
change below current reserved usage returns HTTP 409
`quota_below_current_usage`; malformed or out-of-range values return HTTP 400
`invalid_quota`. Updates are audited as `tenant.quota.update`.

`hako tenant quota get <tenant-id>` reads the limits, and
`hako tenant quota set <tenant-id> <workspaces 1-10> <workload-runs 1-5> [artifact-storage-GiB 1-100 [service-memory-GiB 1-2 service-millicpu 1-1500 [preview-runs 1-5 preview-lifetime-hours 1-24]]] [--queued-workload-runs 1-5] [--workspace-compute-cpu-millicores 1-80000] [--workspace-compute-memory-mib 1-163840] [--no-workspace-compute-limits] [--yes]`
updates them. Before mutation, the CLI reads the current limits and aggregate
usage and displays both the current reservations and requested limits. It
applies the change only after `y` or `yes`; `--yes` skips the prompt for
automation. EOF, `n`, and any other answer cancel without sending the update.
Omitted optional limits preserve the current setting; an
unconfigured Tenant uses 10 GiB Artifact, 2 GiB Service memory, and 1500mCPU
defaults. Preview lifetime may be lowered to 5 minutes. Tenant members can read aggregate usage through
`GET /v1/tenants/{tenant_id}/usage` / `hako usage`, but only Owners/Admins may
read or change quota configuration.

Workload creation reserves both a concurrent Run slot and, for `pending` and
`queued` states, a bounded queue slot transactionally under the same Tenant
quota OCC row. Queue admission failure rolls back the Run, artifact reservation,
audit, and scheduling Outbox together. Terminal transition releases both
reservations; moving into `provisioning` releases only the queue slot. Queue
usage is visible in the Tenant usage response and `hako usage`.

Workspace, Workload, and Preview admission advance the same per-Tenant quota revision
row before checking usage. Quota updates also write that row in the same
transaction, so admission and reconfiguration conflict/retry under Aurora
DSQL OCC. Service admission and accepted stop/delete reports advance this same
Tenant revision before their per-Workspace reservation guard. Service caps are
per Workspace, not aggregate across all Tenant Workspaces. Existing
reservations are not revoked by configuration changes; the API rejects settings
that would put any Workspace's current Service reservations above the new cap.
For each newly admitted Workload Run, Hako reserves up to 100 MiB for its
bounded redacted log artifact. This reservation competes with retained artifact
bytes under the same Tenant OCC guard, so concurrent Runs cannot consume the
same capacity. A successful artifact registration converts reserved capacity
to stored metadata bytes; terminal Runs without artifacts release the full
reservation. Existing active Runs receive the same reservation during
migration. Usage reports `artifact_bytes_used` and `artifact_bytes_reserved`
separately; both count against the configured limit. Artifact usage is based on
registered retained metadata, not a live S3 inventory or billing measurement.
If available artifact capacity cannot cover the maximum per-Run reservation,
Workload creation returns HTTP 409 `artifact_quota_exceeded` before dispatch.
Expired metadata still counts until an authorized artifact/object cleanup path
deletes it. Preview Runs have an independent Tenant concurrency limit in
addition to the general Workload limit. Pending, queued, provisioning, running,
and cancel-requested Workload Previews count until a terminal transition
commits; timeout is enforced through the Workload Run expiration worker. The
same Preview limit is shared with opted-in GitHub PR Previews. Open/reopen
admission and webhook lifecycle state update share the Tenant OCC transaction.
When PR capacity is full, the event is retained as `quota_exceeded` without
runtime allocation; its slot remains reserved through `cleanup_pending` until
physical cleanup is confirmed. Usage reports include those PR reservations.
Quota reconfiguration is also rejected if a lower Preview count would fall
below active PR reservations or if a shorter lifetime would invalidate an
active PR Preview's recorded expiry. A rejected update leaves the previous
limits intact.
Direct Workload Preview creation exceeding the Tenant count or lifetime limit
returns HTTP 409 `preview_quota_exceeded`. The store provides a bounded
expired-metadata listing and an
exact-reference, expiry-conditional metadata delete for a cleanup worker. The
worker must delete the backing object first and call the metadata delete only
after success; failed object deletion must leave metadata and quota usage intact.
The bounded Resource Plane cleanup core now validates a single configured
bucket/prefix, deletes the object before reporting metadata cleanup, continues
past per-object failures, and never connects directly to Control Plane DSQL.
The Control Plane now exposes HMAC-authenticated, Resource-Plane-scoped
candidate listing and idempotent cleanup acknowledgement endpoints; the client
signs both the listing query and acknowledgement body. Expired objects are not
automatically removed until the default-off Resource Plane Lambda schedule is
explicitly deployed. Local PostgreSQL E2E verifies cross-Plane candidate
isolation, signed requests, object-delete-before-metadata-release, and
idempotent acknowledgement. Because the Artifact bucket is externally owned,
review versioning and lifecycle configuration: `DeleteObject` permanently
removes the current key in an unversioned bucket, but a versioned bucket retains
prior versions unless they are separately expired. These limits describe
configured reservations, not measured resource use or billing. Enabling the
schedule also requires the Control Plane's
`resource_plane_artifact_cleanup_secret_arns` entry to reference this Plane's
dedicated cleanup HMAC Secret; cross-account access requires an operator-owned
Secret resource policy for the exact Control Plane API role.
