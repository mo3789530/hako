# Resource Controller Lambda

`cmd/hako-resource-controller-lambda` is the Lambda adapter for the existing
Resource Controller. It accepts SQS event batches, invokes the shared command
handler, and returns `ReportBatchItemFailures` for only the messages whose
command handling or result publication failed. A successful message is
considered complete only after its result has been sent to the Resource Plane
result queue. The Terraform event source mapping uses batch size one today so
that one Workspace operation cannot consume the Lambda timeout budget of
several other records.

## Runtime selection and deployment boundary

The binary requires an explicit `HAKO_RUNTIME_IMPLEMENTATION` value. `fake`
uses the in-memory test Runtime. `lambda-microvm` composes the Workspace
Lambda MicroVM adapter with the S3 conditional journal, a Resource
Plane-owned static launch profile, and the AWS Lambda MicroVM SDK client. The
launch profile is provided through
`HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON`; the journal uses
`HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET` and optional
`HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX`. Optional poll settings are
`HAKO_WORKSPACE_RUNTIME_POLL_INTERVAL` (default `500ms`, range `10ms`–`10s`)
and `HAKO_WORKSPACE_RUNTIME_POLL_TIMEOUT` (default `30s`, range `1s`–`10m`).
Unknown JSON fields, fixed per-Workspace hook payloads, unsupported classes,
cross-region images/connectors, and launch roles/connectors from a different
account are rejected. Commands cannot supply image, role, or connector values.

The Resource Plane Terraform module still deploys only the fake mode, tags it
as fake-only, and grants no journal or MicroVM API permissions. The real mode
is not enabled or deployable from the current Terraform configuration. Its
local config/constructor tests make no AWS calls; no Lambda MicroVM, EFS,
network, or Workspace was created. Do not enable the real mode until the Task
43/44/45 image, network, storage, IAM, and runtime PoC gates are reviewed and
explicitly approved. The Lambda adapter itself uses durable revision/tombstone
fencing before provider effects, but end-to-end Workspace durability and
agent/service recovery are not yet available.

The Terraform module creates this function and its SQS event source mapping
only when `enable_fake_resource_controller=true`; the default is `false`. The
function environment fixes `HAKO_RUNTIME_IMPLEMENTATION=fake`, is tagged
`hako:runtime=fake-only`, and runs with a dedicated
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
commands include a Workspace revision. The Lambda MicroVM Runtime persists
and atomically checks that watermark (including delete tombstones) in the S3
journal before provider side effects; the Terraform-deployed Fake Runtime
keeps its watermark only in process memory. The result consumer also refuses
results for superseded revisions.

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
