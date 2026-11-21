# Resource Plane registration manifest

`config/resource-planes.example.json` defines the version-1 static bootstrap
format for Resource Plane metadata. It is non-secret Control Plane
configuration, validated by `internal/resourceplane.Parse`. Do not put AWS
credentials, private keys, session tokens, or database passwords in this file.
The provider's AWS account and region are explicit so operators can compare
the intended isolation boundary with the credentials used to provision that
plane.

Each registration contains:

| Field | Meaning |
| --- | --- |
| `id` | Stable 1-32 character lowercase Hako ID, e.g. `rp-tokyo-01`. |
| `provider` | Version 1 accepts `aws`. |
| `account_id` | 12-digit AWS account ID, never a credential. |
| `region` | AWS Region identifier. |
| `capabilities` | Unique supported capabilities; `microvm` is required. |
| `command_queue_url` | SQS queue URL for Dispatcher commands. |
| `result_queue_url` | SQS queue URL for Resource Plane results. |
| `workload_command_queue_url` | Optional SQS queue URL dedicated to Workload attempt dispatch/cancel. Must be configured together with `workload_result_queue_url`. |
| `workload_result_queue_url` | Optional SQS queue URL dedicated to Workload execution results. |
| `actions_runner_startup_queue_url` | Optional SQS queue URL for ephemeral Actions Runner startup claims. At most one Resource Plane may configure this field until runner placement is implemented. |
| `preview_runtime_command_queue_url` | Optional dedicated SQS queue URL for Preview generation provision/cleanup commands. |

The v1 capability vocabulary is `microvm`, `container`, `private-network`, and
`persistent-volume`. Capability strings are trimmed and lowercased during
validation; duplicate or unknown capabilities fail closed. Queue URLs must be
HTTPS, use the registered Region's SQS hostname, identify the registered AWS
account in the URL path, and be distinct across all configured queues. The
optional Workload queue URLs must either both be present or both absent. The
optional Actions Runner startup queue is distinct from all other queues;
multiple runner queues fail closed because selecting a Resource Plane requires
a durable placement policy. Unknown JSON fields, unsupported schema versions,
duplicate Plane IDs, oversized manifests, and trailing JSON values are
rejected.

The Scheduler persists Plane ID, provider, region, and capability JSON in the
`resource_planes` table; operators currently bootstrap that row and its
`resource_plane_status` row using the SQL procedure in
[Scheduler Placement](scheduler-placement.md). The Outbox Dispatcher and
Result Consumer both load this file via `HAKO_RESOURCE_PLANE_MANIFEST`.
Dispatcher routes Workspace commands to `command_queue_url` and Workload
attempt dispatch/cancel to the dedicated Workload queue when configured. It
does not fall back to a Workspace queue for Workload messages; missing routing
leaves the Outbox item retryable. Both use Region-specific SQS clients. The
Dispatcher also routes `repository_preview_runtime.provision` and
`repository_preview_runtime.cleanup` events to the dedicated
`preview_runtime_command_queue_url`, after validating generation, claim,
logical runtime identity, head (for provision), and pinned Plane. It never
falls back to the Workspace command queue.

The Resource Plane Terraform module can optionally create a distinct Preview
command/result queue pair plus DLQs, exact Dispatcher/result-consumer queue
policies, controller IAM, SQS event-source mapping, and DLQ/age alarms. Set
`enable_preview_runtime = true` only with the Lambda MicroVM Workspace runtime
configured and pass the Control Plane `preview_result_consumer_role_arn` as
`control_plane_preview_result_consumer_role_arn`. The stack exposes the four
Preview queue URL/ARN outputs. Register the command queue URL here and pass
the result queue ARN in Control Plane `preview_result_queue_arns`; the
Control Plane Preview result consumer remains separately default-off. No AWS
resources or grants have been applied.

The Control Plane Dispatcher derives the Preview command queue ARN from this
manifest and grants `sqs:SendMessage` only to the configured Preview queue
ARNs. Its Terraform validation also requires each optional Preview queue URL
to match the registered Plane's account and Region. This is separate from the
Control Plane Preview runtime producer, which only writes the transactional
Outbox and has no SQS permissions.

The Resource Plane Lambda consumer allocates the generation-scoped Lambda
MicroVM using the durable Workspace runtime journal, then reports the
provider binding. This still does not checkout the PR's immutable SHA, start a
Guest or application, or publish an application route. Do not enable Preview
sessions until those steps and the scheduled producer/result consumer
deployment are complete. The
runner startup publisher uses `actions_runner_startup_queue_url` when present,
claims a durable runner admission with a bounded lease, and publishes only the
integration/job/attempt/claim key. The Control Plane Dispatcher IAM role is
granted send access only to the exact configured queue ARN. The current
single-queue limit avoids implicit multi-Plane placement. The
Operation Result Consumer concurrently polls each registered `result_queue_url`.
The Workload result consumer uses `workload_result_queue_url` when deployed.
For a migration, the Dispatcher
still accepts the older `HAKO_RESOURCE_PLANE_QUEUE_URLS` map; the Result
Consumer accepts either `HAKO_OPERATION_RESULT_QUEUE_URLS` or the original
single `HAKO_OPERATION_RESULT_QUEUE_URL`. These legacy queue settings cannot
be combined with the manifest. Automatic DSQL registration/status management
and config reload without process restart are not implemented; operators must
keep manifest records and DSQL placement rows consistent manually.

Placement metadata is configured on the DSQL `resource_planes` row, separately
from this transport manifest. `cost_tier` is one of `low`, `standard`, or
`high`; `isolation_tier` is one of `shared`, `dedicated`, or `isolated`.
Existing rows default to `standard`/`shared`. Keep these classifications in
sync with reviewed infrastructure controls: a tier only influences placement
and does not provision resources or provide a technical isolation guarantee.

The command/result queue URLs should come from the Terraform outputs described
in [Resource Plane queues and DLQ](resource-plane-queues-and-dlq.md). Only the
exact Control Plane Dispatcher/Result Consumer roles should be granted access
by the queue policies. Changing a Resource Plane ID or account/region is a
placement/migration change, not an in-place rename.

Run manifest contract tests with:

```sh
go test ./internal/resourceplane
```
