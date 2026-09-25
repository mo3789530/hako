# Resource Plane Capacity Reservations

The Control Plane can impose an admission limit on each Resource Plane using Workspace slots. This is a placement guard, not a VM CPU/RAM scheduler.

## Semantics

- A `resource_plane_capacities` row configures `max_workspaces` and its transactional `reserved_workspaces` count. No row means the Resource Plane remains unbounded for backward-compatible rollout.
- The Scheduler tries candidates in its existing least-loaded order. For configured candidates it increments `reserved_workspaces` only when the counter is below `max_workspaces`; a full candidate is skipped in favor of the next eligible one.
- The counter increment, Workspace reservation record, Placement, Workspace, Operation, and Outbox command commit together. Failed Workspace creation rolls the reservation back.
- `resource_plane_reservations` records the Resource Plane assigned to each new Workspace, including Workspaces on unbounded planes. The reservation remains held for pending, running, failed, and suspended Workspaces; it is released only when the Control Plane observes `deleted`. This preserves capacity for a Workspace's existing Placement when resumed.
- Simultaneous creates contend on the capacity counter update. PostgreSQL row updates serialize the claims; Aurora DSQL OCC conflicts use the existing transaction retry policy. The integration test verifies capacity exhaustion and release/reuse.
- If all otherwise eligible Resource Planes are full, Workspace creation returns HTTP 503 `resource_plane_unavailable`; no Workspace, Tenant quota slot, or capacity reservation is committed.

## Configuration and migration

Migrations `000021` and `000022` create the capacity and reservation tables. Capacity is currently operator-configured in the Control Plane DB; there is no public Resource Plane capacity management API yet.

For a new/empty Resource Plane, configure its Workspace-slot ceiling before enabling it for placement:

```sql
INSERT INTO resource_plane_capacities
    (resource_plane_id, max_workspaces, reserved_workspaces, updated_at)
VALUES
    ('rp-tokyo-01', 100, 0, now());
```

When adding a capacity limit to a Resource Plane that already has Workspaces, backfill reservation records and initialize the count before enabling the limit. Do this while Workspace creates are paused for that Plane:

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

Set `max_workspaces` at least as high as the current reservation count. Lowering the ceiling below the current count is allowed by the schema but blocks new reservations until usage falls below the new ceiling; existing Workspaces are not evicted. Removing a capacity row disables admission limiting for that Plane. Do not manually change `reserved_workspaces` during normal operation; reconcile it from non-deleted reservations during maintenance if operational repair is needed.

## Deliberate limitations

Each Workspace currently consumes exactly one slot regardless of Runtime Class. Suspended Workspaces keep their slot. This does not measure physical VM capacity, vCPU, memory, EFS, or provider quotas. Per-tenant reservations, runtime-class weights, health-informed capacity sizing, cost-aware placement, isolation tiers, and an operator API remain future work. Health-based candidate preference/exclusion is documented in [Resource Plane Health](resource-plane-health.md).
