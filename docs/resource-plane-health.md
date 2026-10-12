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

Migration `000023` creates `resource_plane_health`. Authorized platform administrators can read/update a report through the Control Plane API:

```http
GET /v1/admin/resource-planes/{resource_plane_id}/health
PUT /v1/admin/resource-planes/{resource_plane_id}/health
Authorization: Bearer <Cognito access token with hako/api scope and hako-admin group>
Content-Type: application/json

{"status":"degraded","reason":"elevated startup failures"}
```

Only a verified Cognito `hako-admin` group claim can access these routes; Tenant Owner/Admin membership does not grant platform administration. The handler resolves the verified Cognito subject to a Hako User. `status` must be `healthy`, `degraded`, or `unhealthy`; `reason` is trimmed, limited to 500 UTF-8 bytes, and rejects control characters. The server assigns observation time, so clients cannot backdate or future-date a report. Updating health and appending `platform_audit_events` commit atomically; audit failure returns an error and rolls back the health change. Audit details record the status but intentionally omit free-form reason text. Create/manage the Cognito group and group membership outside the dev Terraform module, which only consumes a pre-existing User Pool.

An unreported Resource Plane returns `status: unknown` and `effective_status: healthy` to preserve the existing placement default. A stale healthy report returns `effective_status: degraded`; stale unhealthy remains excluded. The GET response includes the stored `reported_at` timestamp and reason when present.

Migrations `000033` and `000034` distinguish operator and automated report sources and add append-only `resource_plane_health_events` history; migrations `000075`–`000076` add report-ID replay protection. The signed ingress is `POST /v1/internal/resource-planes/{resource_plane_id}/health` and is registered only when the API is given a Resource Plane health-report key resolver. It does not use Cognito: the Resource Plane identity is the route-bound ID, authenticated by a per-Plane HMAC key of at least 32 bytes. The signature is lowercase/uppercase hex HMAC-SHA256 over:

```text
HTTP_METHOD + "\n" + escaped_path + "\n" + unix_timestamp + "\n" + report_id + "\n" + hex(SHA256(raw_body))
```

Headers are `X-Hako-Timestamp`, `X-Hako-Report-ID`, and `X-Hako-Signature`. The body is strict JSON with `status`, `reason`, and RFC3339 `observed_at`; maximum body size is 4 KiB. Request timestamps more than five minutes old or 30 seconds in the future are rejected. `report_id` is unique per Resource Plane: an exact retry returns success with `accepted: false`; changed content under the same ID, too-old/future observations, or out-of-order observations are rejected. The API stores events and updates current health in one transaction. Its key resolver must use a protected secret source; keys are never stored in DSQL, and key material must never be logged.

The protocol and database path are implemented and PostgreSQL integration-tested, including invalid HMAC, accepted report, duplicate replay, changed-content conflict, stale/future timestamps, and out-of-order delivery. The HTTPS client signs requests, applies a hard timeout, refuses redirects, bounds response reads, and surfaces reporter outages without logging response bodies. A probe aggregation library runs bounded concurrent checks and reports `degraded` for partial failure, `unhealthy` when all checks fail, and `healthy` on recovery; it records failed probe names only, not arbitrary provider error strings. The optional ARM64 Lambda reporter runs once per minute and checks command/result SQS queue metadata, exact configured Lambda MicroVM image ARN/version, exact VPC egress connector ARN, exact Controller function ARN, and exact Workspace EFS filesystem ARN. Image checks require matching identity, successful build state, and `ACTIVE` launch status; connector checks require matching ARN and `ACTIVE` state; Controller checks require the exact function ARN to be `Active` with no failed update; EFS checks require the exact filesystem, `available`, encrypted, and at least one mount target. These metadata checks do not validate queue message processing, a launched MicroVM, connector traffic/policy, or a successful EFS mount from a Workspace. `MetricsForAttempt` plus the EMF encoder emits the namespace `Hako/ResourcePlaneHealth`: `HealthReportHeartbeat`, `HealthUnhealthy`, `ProbeFailures`, and `ReporterFailures`, dimensioned by `ResourcePlaneId`. Resource Plane Terraform has default-off alarms/log retention: missing heartbeat fails closed, consecutive unhealthy reports alert, probe failures alert, and the unhealthy alarm's OK transition indicates recovery.

The Control Plane can be configured with an exact JSON map from Resource Plane IDs to Control Plane-account HMAC Secret ARNs. Only those ARNs receive `secretsmanager:GetSecretValue`; the API resolves keys only when handling a signed report. A nonempty map creates the API Gateway `NONE` route with per-route throttling; an empty map keeps it disabled. Resource Plane Terraform optionally deploys the scheduled reporter and a dedicated role that can read only its exact Plane Secret, read metadata for only the command/result queues, call `lambda:GetMicrovmImageVersion` for only the configured image ARN, `lambda:GetNetworkConnector` for only the configured egress connector ARN, `lambda:GetFunctionConfiguration` for only the configured Controller function ARN, and `elasticfilesystem:DescribeFileSystems` for only the configured EFS filesystem ARN, then write only to its retained log group. EMF metrics are extracted from logs, so the reporter does not have `cloudwatch:PutMetricData`. Terraform receives only ARN/version metadata, never Secret values. Provision a separate Secret copy in each account with identical random key bytes; automated rotation is not yet implemented and both copies must be rotated together. This implementation is opt-in and statically validated only: no reporter was deployed, no Secret was fetched, and no live AWS capability probes were run. The existing admin API remains the operator signal, not proof that all runtime capabilities are reachable. Direct DB changes bypass source history and API audit and should be reserved for controlled repair. A legacy SQL example is:

```sql
INSERT INTO resource_plane_health (resource_plane_id, status, reason, updated_at)
VALUES ('rp-tokyo-01', 'degraded', 'elevated startup failures', now())
ON CONFLICT (resource_plane_id) DO UPDATE
SET status = EXCLUDED.status,
    reason = EXCLUDED.reason,
    updated_at = EXCLUDED.updated_at;
```

Use `healthy`, `degraded`, or `unhealthy`. `reason` is an operator/monitor note, not user-visible data. The database row does not expire automatically, but the Scheduler applies the effective-health TTL above. Monitoring should explicitly set `unhealthy` when it can no longer vouch for a Plane. Deleting a row restores the compatibility default of healthy, so do not delete an unhealthy row as a recovery action without confirming the Plane is healthy.

The AWS metadata probes use the separate [Lambda MicroVM `GetMicrovmImageVersion` API](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_GetMicrovmImageVersion.html), [Lambda Core `GetNetworkConnector` API](https://docs.aws.amazon.com/lambda/latest/lambda-core/API_GetNetworkConnector.html), [Lambda `GetFunctionConfiguration` API](https://docs.aws.amazon.com/lambda/latest/api/API_GetFunctionConfiguration.html), and [EFS `DescribeFileSystems` API](https://docs.aws.amazon.com/efs/latest/APIReference/API_DescribeFileSystems.html). EFS accepts a specific file-system ID/ARN to avoid listing all filesystems; the IAM action supports file-system resource scoping in the [EFS Service Authorization Reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_efs.html). A successful metadata probe is not a substitute for an approved live connectivity/isolation PoC.

The health signal does not change lifecycle status or reconcile existing Workspace state. Until a scheduled reporter and alarms are deployed, platform operators must monitor Resource Plane health manually and use the admin API to set a Plane unhealthy or lifecycle `draining`/`disabled` when appropriate. To disable automated reporting safely after deployment: first remove/disable the scheduled probe invocation, then disable its API Gateway route/key resolver, retain the last health row (especially `unhealthy`), and verify the Plane is drained or disabled before any repair. Never delete an unhealthy row to recover placement; only a fresh verified healthy report or audited operator update restores it. The reporter must not change lifecycle status.
