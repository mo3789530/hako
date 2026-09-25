# Resource Plane Health

Resource Plane lifecycle (`active`, `draining`, `disabled`) and current health are separate signals. The Scheduler uses health to avoid new placements on known-bad planes and prefer healthy capacity.

## Selection behavior

| Health state | New Workspace placement |
| --- | --- |
| `healthy` | Preferred over degraded candidates |
| `degraded` | Eligible only after all otherwise eligible healthy candidates; still subject to capacity and Tenant policy |
| `unhealthy` | Excluded |
| No health row | Treated as `healthy` for backward compatibility |

Health reports have a five-minute freshness TTL. A stale `healthy` report is
treated as `degraded`, so it loses preference to a fresh healthy Plane but can
still be used as fallback. A stale `degraded` report remains degraded, and a
stale `unhealthy` report remains excluded: silence must not silently re-enable
a Plane last reported unhealthy. The TTL does not rewrite the stored row; a
fresh report or an operator update changes its effective placement health.

Within a health class, the Scheduler continues least-loaded balancing. A full healthy Plane is skipped and a degraded Plane may be used if it has available configured capacity. If all eligible Planes are unhealthy, or all healthy/degraded candidates are otherwise unavailable, Workspace creation returns HTTP 503 `resource_plane_unavailable`. Existing Workspaces are not moved or stopped when health changes.

## Updating health

Migration `000023` creates `resource_plane_health`. There is not yet an automated health reporter or public health-management API; authorized operators update this table through the Control Plane database's administrative access. Set `updated_at` to the time the observation was actually made, not a future timestamp. Example:

```sql
INSERT INTO resource_plane_health (resource_plane_id, status, reason, updated_at)
VALUES ('rp-tokyo-01', 'degraded', 'elevated startup failures', now())
ON CONFLICT (resource_plane_id) DO UPDATE
SET status = EXCLUDED.status,
    reason = EXCLUDED.reason,
    updated_at = EXCLUDED.updated_at;
```

Use `healthy`, `degraded`, or `unhealthy`. `reason` is an operator/monitor note, not user-visible data. The database row does not expire automatically, but the Scheduler applies the effective-health TTL above. Monitoring should explicitly set `unhealthy` when it can no longer vouch for a Plane. Deleting a row restores the compatibility default of healthy, so do not delete an unhealthy row as a recovery action without confirming the Plane is healthy.

The health signal does not change lifecycle status, reconcile existing Workspace state, or represent an AWS API health probe by itself. Automated health collection, a public health-management API, audit export, and recovery policy are future work.
