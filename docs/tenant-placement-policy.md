# Tenant Placement Policy

Tenant Owner/Admin can constrain where new Workspaces are placed. The policy is Control Plane state stored in `tenant_placement_policies`; it is not Resource Plane configuration and does not grant a Tenant direct AWS access.

## API

`GET /v1/tenants/{tenant_id}/placement-policy` returns the configured policy. A Tenant without an explicit row receives empty selector lists, which mean no additional Tenant restriction.

`PUT /v1/tenants/{tenant_id}/placement-policy` replaces the policy:

```json
{
  "allowed_regions": ["ap-northeast-1"],
  "resource_plane_ids": ["rp-tokyo-01"],
  "required_capabilities": ["microvm"],
  "max_cost_tier": "standard",
  "minimum_isolation_tier": "dedicated"
}
```

Only Tenant Owner and Admin roles can read or change the policy. Other members and users outside the Tenant receive the same non-disclosing `404 Tenant not found` response used by other tenant-scoped authorization checks. Unknown JSON fields, empty or duplicate normalized selectors, and lists over 50 entries are rejected with HTTP 400 `invalid_request`.

The CLI exposes the same operations:

```sh
hako tenant placement-policy get tenant-acme
hako tenant placement-policy set tenant-acme '{"allowed_regions":["ap-northeast-1"],"resource_plane_ids":["rp-tokyo-01"],"required_capabilities":["microvm"]}'
```

`HAKO_API_URL` and a token from `hako login` are required. The `set` command replaces all selector lists and both tier constraints; omit or use an empty string for either tier to remove that constraint.

## Scheduler semantics

- An empty selector is unrestricted; a non-empty `allowed_regions` or `resource_plane_ids` list is an allow-list.
- Tenant allow-lists intersect with server configuration and internal create constraints; they cannot override a server-side region/plane pin.
- Tenant `required_capabilities` are added to the requirements from the API/runtime configuration. Every required capability must exist on an active Resource Plane.
- `max_cost_tier` is an optional hard ceiling (`low`, `standard`, or `high`). When set, lower-cost candidates are preferred within each effective-health class, then compared by Workspace load and stable region/ID tie-breaks. Health class remains the higher-priority signal. With no ceiling, the existing load-balanced order is preserved.
- `minimum_isolation_tier` is an optional floor (`shared`, `dedicated`, or `isolated`). A candidate below the floor is excluded. This is a scheduling classification only; it does not itself provision dedicated infrastructure or prove an isolation boundary.
- Policy is read inside the Workspace create transaction before Placement, Operation, and Outbox records are written. If no active Resource Plane satisfies all constraints, creation returns HTTP 503 `resource_plane_unavailable` and commits no Workspace or quota slot.
- The policy affects new placement only. Changing it does not move existing Workspaces.

## Scope and limitations

Tenant allow-lists, capability requirements, cost ceilings, isolation floors, and Resource Plane Workspace/CPU/RAM reservations are implemented. Health freshness is evaluated with a five-minute TTL: stale healthy reports become degraded fallbacks, while unhealthy remains excluded. Cost tiers are relative operator labels, not currency prices or billing data. Operators must accurately classify each Plane and separately enforce the infrastructure isolation represented by its tier. See [Resource Plane Capacity Reservations](resource-plane-capacity.md), [Resource Plane Health](resource-plane-health.md), and [Scheduler and Workspace Placement](scheduler-placement.md).

See also [Scheduler and Workspace Placement](scheduler-placement.md).
