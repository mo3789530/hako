# Scheduler and Workspace Placement

Workspace creation now selects a Resource Plane inside the same database transaction that inserts the Workspace, Placement, initial Operation, event, and Outbox command. The chosen `resource_plane_id` is persisted on both Placement and Operation, so the Dispatcher can route the command without a second placement lookup.

## MVP selection policy

The API supplies `microvm` as a required capability. Optional `HAKO_DEFAULT_RESOURCE_PLANE_REGION` is a hard region constraint, not a soft preference. Candidates must have a `resource_plane_status` row with status `active`; a missing status row is not eligible. Each candidate's `capabilities_json` must contain every required capability. Health status is independent: healthy is preferred, degraded is a fallback, unhealthy is excluded, and a missing health row defaults to healthy. See [Resource Plane Health](resource-plane-health.md).

Among eligible candidates, the Scheduler selects the one with the fewest placed Workspaces whose Observed State is not `deleted`. Ties are deterministic by region and Resource Plane ID. Deleted Workspaces do not contribute to this load.

If no candidate is eligible, creation returns HTTP 503 `resource_plane_unavailable`; it does not reserve quota or create a Workspace/Operation/Outbox record. Operators must register active Resource Planes and their capabilities before serving create requests.

## Configuration

The API no longer needs `HAKO_DEFAULT_RESOURCE_PLANE_ID`. Configure `HAKO_DEFAULT_WORKSPACE_IMAGE`; optionally constrain placement with `HAKO_DEFAULT_RESOURCE_PLANE_REGION`. Runtime Class remains `HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS` (default `standard`).

Example local Resource Plane bootstrap:

```sql
INSERT INTO resource_planes (id, provider, region, capabilities_json)
VALUES ('rp-local', 'aws', 'ap-northeast-1', '["microvm"]');

INSERT INTO resource_plane_status (resource_plane_id, status, updated_at)
VALUES ('rp-local', 'active', now());
```

## Deliberate limitations

Tenant placement policy is loaded in the same create transaction and intersects with server-side constraints. Configured Resource Plane Workspace-slot capacity is also reserved atomically during selection and released on observed deletion. See [Tenant Placement Policy](tenant-placement-policy.md) and [Resource Plane Capacity Reservations](resource-plane-capacity.md). This initial Scheduler still does not model isolation tier, automated health checks/freshness, cost, or CPU/RAM capacity; its balancing signal remains the count of non-deleted Workspaces within each health class. Independent status changes may race with create; the Resource Controller/Reconciler must handle later Resource Plane unavailability.
