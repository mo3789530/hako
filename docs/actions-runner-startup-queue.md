# Actions Runner startup queue

Status: local Control Plane dispatch composition and Resource Plane consumer
handler are implemented. Queue provisioning, event-source mapping, exact IAM,
multi-Resource-Plane placement, and live AWS delivery are not implemented.
Runner admission and dispatch remain opt-in.

## Flow

```text
workflow_job.queued
  -> durable admitted row
  -> scheduled Control Plane Outbox Dispatcher
  -> bounded DB claim lease
  -> dedicated SQS startup queue
  -> Resource Plane Lambda partial-batch handler
  -> mTLS broker claim consumption
  -> Lambda MicroVM launch / bind / bootstrap
```

The scheduled Outbox Dispatcher runs the runner admission publisher only when
`HAKO_ACTIONS_RUNNER_STARTUP_QUEUE_URL` is configured. The optional
`HAKO_ACTIONS_RUNNER_STARTUP_QUEUE_REGION` selects the signing region; if
unset, the Lambda's configured AWS region is used. The publisher claims at most
10 admitted/recoverable jobs per invocation with a 10-minute lease. Each SQS
message contains only `schema_version`, `integration_id`, `workflow_job_id`,
`run_attempt`, and `claim_id`; Tenant, Repository, commit, labels, and
credentials are resolved later by the trusted broker from the database.

Delivery is at-least-once. The database claim is committed before `SendMessage`
is called, so a failed or ambiguous send is recovered after lease expiry and
may result in duplicates. The Resource Plane broker accepts a claim only while
it is live; same-Plane replay before token issuance is idempotent. The SQS
consumer returns per-record failures, allowing successful records in a batch
to remain acknowledged. FIFO queues use deterministic group/deduplication IDs;
standard queues rely on claim fencing.

This composition currently accepts one explicitly configured startup queue.
It is suitable only for a single configured Resource Plane; it does not yet
perform capacity/region placement or persist a selected Plane before dispatch.
Multi-plane routing must add a durable placement decision and per-Plane queue
mapping before production use.

## Local verification

```sh
GOCACHE=/tmp/hako-go-cache go test ./internal/actionsrunner ./cmd/hako-dispatcher-lambda
```

Tests use fake claim sources, fake SQS, and fake runner dependencies. They do
not call AWS, read real Secrets, or require SQS. The runner Lambda entrypoint
and ARM64 ZIP build are documented in
[`tasks/task-52-ephemeral-actions-runner.md`](../tasks/task-52-ephemeral-actions-runner.md).

## Not yet deployable

- Provision a dedicated encrypted startup queue and DLQ with a bounded
  visibility timeout and redrive policy.
- Grant the Control Plane Dispatcher only `sqs:SendMessage` to that exact queue;
  grant the Resource Plane Lambda only receive/delete/visibility actions.
- Add an opt-in event-source mapping with `ReportBatchItemFailures`, and prove
  queue visibility exceeds Lambda timeout and startup operation bounds.
- Add durable multi-Plane placement and queue selection; do not route by
  untrusted workflow labels.
- Verify restricted egress, runner image/listener behavior, and cleanup under
  approved AWS/GitHub test credentials before enabling admission.
