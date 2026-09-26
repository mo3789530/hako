# Resource Plane Capacity Reservations

The Control Plane can impose Workspace-slot and optional CPU/RAM admission limits on each Resource Plane. These are transactional placement reservations, not measurements of live VM utilization or provider quota discovery.

## Semantics

- A `resource_plane_capacities` row configures `max_workspaces` and its transactional `reserved_workspaces` count. No row means the Resource Plane remains unbounded for backward-compatible rollout.
- The Scheduler tries candidates in its existing least-loaded order. For configured candidates it increments `reserved_workspaces` only when the counter is below `max_workspaces`; a full candidate is skipped in favor of the next eligible one.
- The counter increment, Workspace reservation record, Placement, Workspace, Operation, and Outbox command commit together. Failed Workspace creation rolls the reservation back.
- `resource_plane_reservations` records the Resource Plane assigned to each new Workspace, including Workspaces on unbounded planes. The reservation remains held for pending, running, failed, and suspended Workspaces; it is released only when the Control Plane observes `deleted`. This preserves capacity for a Workspace's existing Placement when resumed.
- Simultaneous creates contend on the capacity counter update. PostgreSQL row updates serialize the claims; Aurora DSQL OCC conflicts use the existing transaction retry policy. The integration test verifies capacity exhaustion and release/reuse.
- `runtime_class_resources` maps each Runtime Class to operator-defined CPU millicores and memory MiB. When a Plane has either compute ceiling configured, an unmapped Runtime Class fails closed instead of being admitted without a known demand.
- Reservation rows snapshot the Runtime Class demand used at creation. Deletion releases that exact snapshot even if the class profile is later edited. Profile changes do not resize already-created Workspaces.
- `max_cpu_millicores` and `max_memory_mib` are independent optional limits; `NULL` means that dimension is not admission-limited. Workspace count remains an independent limit.
- If all otherwise eligible Resource Planes are full, Workspace creation returns HTTP 503 `resource_plane_unavailable`; no Workspace, Tenant quota slot, or capacity reservation is committed.

## Configuration and migration

Migrations `000021` and `000022` create the capacity and reservation tables. Migrations `000028`–`000030` add optional compute ceilings, Runtime Class demand profiles, and per-reservation demand snapshots. Capacity/profile data is currently operator-configured in the Control Plane DB; there is no public management API yet.

For a new/empty Resource Plane, configure its Workspace-slot ceiling before enabling it for placement:

```sql
INSERT INTO resource_plane_capacities
    (resource_plane_id, max_workspaces, reserved_workspaces, updated_at)
VALUES
    ('rp-tokyo-01', 100, 0, now());
```

To add a compute ceiling, first map every Runtime Class that can be placed on
the Plane. For example, the architecture describes `standard` as up to 8 vCPU
and 16 GiB; millicores use 1000 per vCPU and MiB use 1024 per GiB:

```sql
INSERT INTO runtime_class_resources
    (runtime_class, cpu_millicores, memory_mib, updated_at)
VALUES
    ('standard', 8000, 16384, now());

UPDATE resource_plane_capacities
SET max_cpu_millicores = 64000,
    max_memory_mib = 131072,
    updated_at = now()
WHERE resource_plane_id = 'rp-tokyo-01';
```

Treat these as configured admission weights, not observed utilization. Define
profiles for `small`, `large`, and custom classes according to their actual
Runtime contract before placing them on a compute-limited Plane. A missing
profile on a weighted Plane fails closed and returns a server-side placement
configuration error; do not rely on a zero-cost fallback.

When adding any capacity limit to a Resource Plane that already has Workspaces, backfill reservation records and initialize counters before enabling the limit. For compute limits, configure all class profiles and verify no existing Workspace class is unmapped first. Pause creates and deletes for that Plane during this maintenance:

```sql
INSERT INTO resource_plane_reservations (workspace_id, resource_plane_id, reserved_at)
SELECT p.workspace_id, p.resource_plane_id, p.placed_at
FROM placements p
JOIN workspace_status s ON s.workspace_id = p.workspace_id
WHERE p.resource_plane_id = 'rp-tokyo-01'
  AND s.observed_state <> 'deleted'
ON CONFLICT (workspace_id) DO NOTHING;

INSERT INTO resource_plane_capacities
    (resource_plane_id, max_workspaces, reserved_workspaces, updated_at)
SELECT 'rp-tokyo-01', 100, COUNT(*), now()
FROM resource_plane_reservations
WHERE resource_plane_id = 'rp-tokyo-01'
ON CONFLICT (resource_plane_id) DO UPDATE
SET max_workspaces = EXCLUDED.max_workspaces,
    reserved_workspaces = EXCLUDED.reserved_workspaces,
    updated_at = EXCLUDED.updated_at;
```

For a compute-limited Plane, snapshot demand for legacy reservations and
recompute the counters before setting CPU/RAM ceilings:

```sql
UPDATE resource_plane_reservations r
SET runtime_class = LOWER(BTRIM(w.runtime_class)),
    cpu_millicores = c.cpu_millicores,
    memory_mib = c.memory_mib
FROM workspaces w
JOIN runtime_class_resources c ON c.runtime_class = LOWER(BTRIM(w.runtime_class))
WHERE r.workspace_id = w.id
  AND r.resource_plane_id = 'rp-tokyo-01'
  AND r.cpu_millicores IS NULL;

UPDATE resource_plane_capacities p
SET reserved_cpu_millicores = COALESCE((
        SELECT SUM(r.cpu_millicores) FROM resource_plane_reservations r
        WHERE r.resource_plane_id = p.resource_plane_id
    ), 0),
    reserved_memory_mib = COALESCE((
        SELECT SUM(r.memory_mib) FROM resource_plane_reservations r
        WHERE r.resource_plane_id = p.resource_plane_id
    ), 0),
    updated_at = now()
WHERE p.resource_plane_id = 'rp-tokyo-01';
```

Before updating counters, confirm no non-deleted reservation remains with a
NULL demand; an unmapped legacy class must be resolved explicitly. Resume
Workspace changes only after the reconciled counters and new ceilings are
reviewed.

Set `max_workspaces` at least as high as the current reservation count. Lowering the ceiling below the current count is allowed by the schema but blocks new reservations until usage falls below the new ceiling; existing Workspaces are not evicted. Removing a capacity row disables admission limiting for that Plane. Do not manually change `reserved_workspaces` during normal operation; reconcile it from non-deleted reservations during maintenance if operational repair is needed.

## Deliberate limitations

The direct PostgreSQL integration tests cover unbounded Planes, CPU/RAM admission, reservation counter updates, duplicate release, and underflow rollback (`go test -tags=integration ./internal/store/resourcecapacity`). Suspended Workspaces keep all reservations. CPU/RAM weights are static operator configuration and do not measure physical VM usage, overcommit, EFS, or provider quotas. Per-tenant reservations and a capacity-management API remain future work. Automated health report persistence now distinguishes operator and machine sources, but reporter authentication/probes/scheduling remain future work. Health-based candidate preference/exclusion is documented in [Resource Plane Health](resource-plane-health.md).
