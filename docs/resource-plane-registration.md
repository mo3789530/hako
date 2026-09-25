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

The v1 capability vocabulary is `microvm`, `container`, `private-network`, and
`persistent-volume`. Capability strings are trimmed and lowercased during
validation; duplicate or unknown capabilities fail closed. Queue URLs must be
HTTPS, use the registered Region's SQS hostname, identify the registered AWS
account in the URL path, and be different for commands and results. Unknown
JSON fields, unsupported schema versions, duplicate Plane IDs, oversized
manifests, and trailing JSON values are rejected.

The Scheduler persists Plane ID, provider, region, and capability JSON in the
`resource_planes` table; operators currently bootstrap that row and its
`resource_plane_status` row using the SQL procedure in
[Scheduler Placement](scheduler-placement.md). The Dispatcher currently
consumes a separate `HAKO_RESOURCE_PLANE_QUEUE_URLS` JSON map of Plane IDs to
command queue URLs, and the Result Consumer accepts one result queue URL. The
manifest parser does not yet load configuration into DSQL, generate that
environment variable, or support polling result queues across multiple
Resource Planes. Until that wiring is implemented, keep these representations
consistent manually and treat the manifest as the reviewed source of truth.

The command/result queue URLs should come from the Terraform outputs described
in [Resource Plane queues and DLQ](resource-plane-queues-and-dlq.md). Only the
exact Control Plane Dispatcher/Result Consumer roles should be granted access
by the queue policies. Changing a Resource Plane ID or account/region is a
placement/migration change, not an in-place rename.

Run manifest contract tests with:

```sh
go test ./internal/resourceplane
```
