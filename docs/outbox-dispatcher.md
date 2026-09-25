# Outbox Dispatcher

`cmd/hako-dispatcher` polls the Control Plane's Transactional Outbox and sends each Operation command to the configured Resource Plane SQS queue. API handlers never send to SQS directly, so Workspace/Operation commits are not coupled to a network call.

## Delivery semantics

- The dispatcher leases due `published_at IS NULL` events in a short database transaction, commits the lease, then sends to SQS.
- If SQS accepts a message but the database acknowledgement fails, the event can be sent again after its lease expires. Delivery is **at least once**.
- Resource Plane consumers must deduplicate by Outbox event ID and/or Operation ID. The Fake Controller's Runtime deduplicates by Operation ID. FIFO queues also receive Operation ID as message group and event ID as deduplication ID; FIFO's dedupe window is not the correctness boundary.
- Send failures and missing queue mappings leave the event unpublished and apply exponential backoff. Expired leases are reclaimable after process crashes.
- Successfully published rows are retained. An Outbox retention/cleanup policy is not defined yet.

## Configuration and running locally

Set `HAKO_DATABASE_URL` for local PostgreSQL or `HAKO_DSQL_HOST` for Aurora DSQL. Map Resource Plane IDs to SQS Queue URLs:

```sh
export HAKO_DATABASE_URL='postgres://hako:local-dev-only@127.0.0.1:5432/hako?sslmode=disable'
export HAKO_RESOURCE_PLANE_QUEUE_URLS='{"rp-tokyo-01":"https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-rp-tokyo-01"}'
export AWS_REGION=ap-northeast-1
go run ./cmd/hako-dispatcher
```

`HAKO_DISPATCHER_INTERVAL` controls polling and defaults to `2s`. The AWS SDK default credential chain supplies credentials; no long-lived access key is configured. The dispatcher role requires `sqs:SendMessage` for each mapped queue. For queues in Resource Plane accounts, their SQS resource policy must also trust the Control Plane role. Queue creation, cross-account resource policies, and IAM deployment are deferred to Terraform work.

## Current boundary

This component delivers SQS commands only. A Fake Resource Plane consumer and in-memory Runtime are available via [`cmd/hako-fake-resource-controller`](fake-resource-controller.md). The Control Plane result consumer now persists terminal results from a separate queue; see [Operation Result Consumer](operation-results.md). No real MicroVM state is changed. An unmapped Resource Plane or unsupported event remains unpublished and is retried with backoff; logs currently provide the primary operational signal.
