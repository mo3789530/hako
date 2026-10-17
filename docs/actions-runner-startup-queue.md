# Actions Runner startup queue

Status: local Control Plane dispatch composition, Resource Plane consumer
handler, and default-off Terraform configuration for the queue/DLQ, cross-
account publisher policy, worker role, Lambda function, and partial-batch
event-source mapping are implemented. AWS resources have not been applied.
Multi-Resource-Plane placement and live AWS delivery remain unimplemented.
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
one Resource Plane manifest entry has an
`actions_runner_startup_queue_url`. The queue URL and signing region come from
that validated entry. The publisher claims at most 10 admitted/recoverable jobs
per invocation with a 10-minute lease. Each SQS
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

The Control Plane manifest accepts at most one Actions Runner startup queue.
Multiple queue entries fail closed until durable placement exists. This is
suitable only for a single configured Resource Plane; it does not yet perform
capacity/region placement or persist a selected Plane before dispatch.
Multi-plane routing must add a durable placement decision and per-Plane queue
mapping before production use.

## Claude Action workflow provenance (implementation in progress)

Claude Code Action remains a user-authored GitHub Actions workflow; Hako does
not translate issue or pull-request comments into commands. The local
`actionsrunner.VerifyRunnerWorkflowProvenance` contract accepts only a
Tenant-owned, data-only policy and authoritative run/job metadata. It compares
Tenant, integration, Installation, repository, workflow file, approved ref,
commit, run ID/attempt, job ID, event, source repository and required runner
labels, and rejects fork-originated work. Policy values cannot select a
command, credential, image, execution role, or network connector.

This verifier is currently a tested library contract only. It is not yet backed
by DSQL policy storage, GitHub Actions Run/Job API reads, or wired before
MicroVM launch. Therefore Claude Action workflows must not be considered
eligible for production runner admission until those checks are integrated.

## Local verification

```sh
GOCACHE=/tmp/hako-go-cache go test ./internal/actionsrunner ./cmd/hako-dispatcher-lambda
```

Tests use fake claim sources, fake SQS, and fake runner dependencies. They do
not call AWS, read real Secrets, or require SQS. The runner Lambda entrypoint
and ARM64 ZIP build are documented in
[`tasks/task-52-ephemeral-actions-runner.md`](../tasks/task-52-ephemeral-actions-runner.md).

## Not yet deployable

- Review and approve the default-off Terraform queue/DLQ, queue policy, worker
  IAM and event-source mapping; apply requires separate user approval.
- Confirm queue visibility exceeds six times the configured Lambda timeout and
  the Lambda timeout exceeds the runner startup operation timeout.
- Add durable multi-Plane placement and queue selection; do not route by
  untrusted workflow labels.
- Build and verify the runner MicroVM image/listener, restricted egress, and
  cleanup under approved AWS/GitHub test credentials before enabling admission.
