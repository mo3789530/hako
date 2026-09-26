# Resource Controller Lambda (development Fake Runtime)

`cmd/hako-resource-controller-lambda` is the Lambda adapter for the existing
Resource Controller. It accepts SQS event batches, invokes the shared command
handler, and returns `ReportBatchItemFailures` for only the messages whose
command handling or result publication failed. A successful message is
considered complete only after its result has been sent to the Resource Plane
result queue. The Terraform event source mapping uses batch size one today so
that one Workspace operation cannot consume the Lambda timeout budget of
several other records.

## Deliberate fake-only boundary

The binary requires `HAKO_RUNTIME_IMPLEMENTATION=fake` and refuses startup if
any other value is set. It uses an in-memory Runtime; no Lambda MicroVM, EFS,
network, or other Workspace resource is created. Its idempotency cache is
process-local and is lost on cold start, so it is not a production Workspace
executor. Schema v2 fencing is checked by the Control Plane and the Fake
Runtime, but production use must wait for a durable/idempotent AWS Runtime
implementation that atomically persists its revision watermark before side
effects, plus compensation/cleanup.

The Terraform module creates this function and its SQS event source mapping
only when `enable_fake_resource_controller=true`; the default is `false`. The
function is tagged `hako:runtime=fake-only`. It runs with a dedicated
pre-created log group and only receives the command queue, sends to the result
queue, and writes to its log streams. No `CreateLogGroup` or runtime-resource
AWS permissions are granted.

Build the Lambda-compatible package (no AWS operation is performed):

```sh
make build-resource-controller-lambda
```

The output is `build/resource-controller-lambda.zip`, suitable for the dev
Terraform input `fake_controller_lambda_zip_path`. To inspect the Terraform
configuration, first build the artifact, set the explicit fake opt-in in an
uncommitted tfvars file, then run `tofu validate`/`plan`. Review the plan before
any apply; applying creates AWS resources and requires an explicitly selected
dev Resource Plane account. This implementation has not been applied to AWS.

## Retry behavior

SQS partial batch responses prevent a poison message or result queue outage
from forcing already-completed messages from the same invocation to replay.
The queue RedrivePolicy still determines when failed messages move to the
DLQ. A duplicate command can still arrive after a Lambda cold start. Schema v2
commands include a Workspace revision, but the current Fake Runtime keeps its
watermark only in process memory. A production Runtime must persist and
atomically check that watermark (including delete tombstones) before resource
side effects; this provider-backed durability is not implemented. The result
consumer also refuses results for superseded revisions.

The adapter also honors `HAKO_COMMAND_MAX_AGE` (default `30m`, positive Go
duration). A well-formed command older than this cutoff is not executed; the
Controller sends an `operation_expired` result and the event is acknowledged
only after that result is delivered. A command timestamp more than five minutes
in the future is rejected for retry/DLQ inspection. Keep the maximum age no
greater than the Control Plane Reconciler timeout. This bounds old queued
messages but cannot cancel a Runtime call that already started. Generation
fencing is separate: the Control Plane drops stale v2 results and the Fake
Runtime checks a process-local monotonic watermark. A real provider Runtime
still needs durable fencing before AWS side effects.

Run the adapter tests with:

```sh
go test ./internal/resourcecontroller
```
