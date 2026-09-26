# Outbox Dispatcher

`cmd/hako-dispatcher` polls the Control Plane's Transactional Outbox and sends each Operation command to the configured Resource Plane SQS queue. API handlers never send to SQS directly, so Workspace/Operation commits are not coupled to a network call.

## Delivery semantics

- The dispatcher leases due `published_at IS NULL` events in a short database transaction, commits the lease, then sends to SQS.
- If SQS accepts a message but the database acknowledgement fails, the event can be sent again after its lease expires. Delivery is **at least once**.
- Resource Plane consumers must deduplicate by Outbox event ID and/or Operation ID. The Fake Controller's Runtime deduplicates by Operation ID. FIFO queues also receive Operation ID as message group and event ID as deduplication ID; FIFO's dedupe window is not the correctness boundary.
- Send failures and missing queue mappings leave the event unpublished and apply exponential backoff. Expired leases are reclaimable after process crashes.
- Successfully published rows are retained. An Outbox retention/cleanup policy is not defined yet.

## Configuration and running locally

Set `HAKO_DATABASE_URL` for local PostgreSQL or `HAKO_DSQL_HOST` for Aurora DSQL. For multiple Resource Planes, point the Dispatcher at the validated registration manifest:

```sh
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
export HAKO_RESOURCE_PLANE_MANIFEST=./config/resource-planes.json
# The manifest provides each plane's command queue URL and Region.
go run ./cmd/hako-dispatcher
```

For a single/migration deployment, `HAKO_RESOURCE_PLANE_QUEUE_URLS` still accepts a JSON map of Plane IDs to URLs. Do not set more than one of the manifest path, manifest JSON, or legacy URL map. `HAKO_DISPATCHER_INTERVAL` controls local process polling and defaults to `2s`. The AWS SDK default credential chain supplies credentials; no long-lived access key is configured. The dispatcher role requires `sqs:SendMessage` for each mapped queue. A Region-specific AWS SDK client is created for each manifest Queue; legacy URL maps use the default `AWS_REGION`. For queues in Resource Plane accounts, their SQS resource policy must also trust the Control Plane role. The queue/DLQ and cross-account queue policies are defined in [Resource Plane queues and DLQ](resource-plane-queues-and-dlq.md), but are not deployed until an operator applies the Resource Plane Terraform root.

## Scheduled Lambda deployment

`cmd/hako-dispatcher-lambda` runs one Outbox batch per EventBridge schedule
invocation. Build the arm64 custom-runtime artifact with
`make build-dispatcher-lambda`. The Control Plane Terraform creates a stable
least-privilege execution role and log group by default. After the Resource
Plane command queue policies trust the output `outbox_dispatcher_role_arn`,
enable `enable_outbox_dispatcher` and provide the versioned manifest JSON. Its
command queue ARNs are derived from that manifest; the role receives only
`sqs:SendMessage` on those queues and `dsql:DbConnect` for the custom
`hako_dispatcher` database role. The SQL bootstrap grants that DB role
`SELECT`/`UPDATE` on `outbox_events` only.

EventBridge triggers the Lambda once per minute, with bounded target retries;
the Outbox remains the durable source of work if an invocation fails. Batch
size is 10 and the lease is two minutes, longer than the Lambda's 60-second
timeout, so a concurrent/retried invocation cannot immediately reclaim a live
batch. Invocation errors raise a CloudWatch alarm. Dispatcher enablement is
opt-in and must follow this order: create Control Plane roles → bootstrap DSQL
custom roles and run migrations → apply Resource Plane queues/policies with the
exact Dispatcher role ARN → provide the queue manifest and enable the Lambda.
Never enable the schedule before the database role and all manifest queues are
ready; otherwise periodic invocations will fail and raise the error alarm.

## Current boundary

This component delivers SQS commands only. A Fake Resource Plane consumer and in-memory Runtime are available via [`cmd/hako-fake-resource-controller`](fake-resource-controller.md). The Control Plane result consumer now persists terminal results from a separate queue; see [Operation Result Consumer](operation-results.md). No real MicroVM state is changed. An unmapped Resource Plane or unsupported event remains unpublished and is retried with backoff; logs currently provide the primary operational signal.
