# Resource Plane command/result queues and DLQ

The `infra/terraform/modules/resource-plane` module defines the initial AWS
message boundary for a Resource Plane. The development root requires explicit
Control Plane role ARNs and must be configured for the intended Resource Plane
AWS account before anyone runs `plan` or `apply`. A development-only fake
Lambda can be enabled for transport tests; the production runtime and Control
Plane dispatcher/consumer deployment remain external.

## Topology

```text
Control Plane Dispatcher role --SendMessage--> commands queue
                                                   |
                                      maxReceiveCount (default 5)
                                                   v
                                          commands DLQ

Resource Controller role --Receive/Delete-------- commands queue
Resource Controller role --SendMessage----------> results queue
                                                   |
                                      maxReceiveCount (default 5)
                                                   v
                                           results DLQ

Control Plane Result Consumer role --Receive/Delete--> results queue

Control Plane Dispatcher role --SendMessage--> workload-commands queue
                                                   |
                                      maxReceiveCount (default 5)
                                                   v
                                          workload-commands DLQ

Workload Controller role --Receive/Delete-------> workload-commands queue
Workload Controller role --SendMessage----------> workload-results queue
Control Plane Workload Result Consumer --Receive/Delete--> workload-results queue
```

The optional `enable_workload_result_queue` (`false` by default) provisions a
separate encrypted Workload command queue/DLQ and result queue/DLQ, independent
alarms, and a dedicated queue-only Workload Controller role. The Control Plane
manifest can include paired `workload_command_queue_url` and
`workload_result_queue_url` fields. The Dispatcher routes Workload attempt
dispatch/cancel only to the dedicated command queue and fails closed (leaving
the Outbox event retryable) when it is absent; Workspace Operation commands
continue to use `command_queue_url`. The Workload Controller role consumes only
the dedicated Workload queue and sends results only to its result queue. It
does not grant access to Workspace queues, Lambda MicroVM APIs, S3, Secrets
Manager, or GitHub broker. The result queue resource policy permits only the
configured exact Control Plane Workload result consumer role. No Workload
Lambda, event source mapping, or Control Plane result consumer is deployed yet.
The opt-in must not be enabled until consumer identities, queue routing, and
worker wiring are reviewed.

With `enable_fake_resource_controller = true`, the command queue can trigger
an optional development-only Lambda using batch size one and
`ReportBatchItemFailures`. A command is acknowledged only after its result has
been sent. The Terraform-deployed function environment is hard-coded to
`HAKO_RUNTIME_IMPLEMENTATION=fake` and therefore uses the in-memory Fake Runtime.
The executable also has a separately configured `lambda-microvm` composition,
but Terraform does not select it or grant its S3 journal/Lambda MicroVM
permissions. It is not a production deployment path yet.

Both primary queues use SQS-managed encryption, 20-second long polling, and a
four-day retention default. Their visibility timeout defaults to 6000 seconds,
which leaves headroom for the Lambda maximum 900-second function timeout plus
the maximum five-minute batching window. Before attaching an event source
mapping, preserve the required relationship between its visibility timeout,
function timeout, and batching behavior. Each DLQ retains messages for
14 days and restricts redrive sources to its corresponding primary queue.
`max_receive_count` is configurable and defaults to five receives.

The Terraform queue resource separates the redrive policy and redrive-allow
policy so the source and permitted source queue are explicit. See the [AWS
provider SQS queue resource](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/sqs_queue.html),
[redrive policy resource](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/sqs_queue_redrive_policy),
and [redrive allow policy resource](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/sqs_queue_redrive_allow_policy).

## IAM and account boundary

The Resource Controller execution role trusts only `lambda.amazonaws.com`. Its
inline policy permits receive/delete/change-visibility/get-attributes on the
Workspace command queue, send on the Workspace result queue, and writes to its
pre-created CloudWatch log group. When Workload queues are enabled, a distinct
Workload Controller role consumes only the Workload command queue, sends only
to the Workload result queue, and writes to its own log group. It has no
permissions to create AWS runtime resources; those permissions will be added
only with a concrete runtime implementation.

The command and optional Workload command queue resource policies trust the
exact configured Control Plane Dispatcher role ARN for `sqs:SendMessage`. The
result queue policy trusts the exact Operation Result Consumer role; the
Workload result policy trusts the exact Workload Result Consumer role for
receive/delete/change-visibility/get-attributes. The Control Plane roles also
need matching identity policies in their own account for cross-account SQS access. DLQs have no cross-account
consumer policy yet; they are intentionally operator-only until an audited
redrive workflow is defined. See the [AWS provider SQS queue policy
resource](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/sqs_queue_policy).

Queue and log resources carry `hako:managed-by` and
`hako:resource-plane-id` tags. Queue-role tags distinguish commands, results,
and their DLQs. Additional environment tags can be supplied through `tags`.

## Dead-letter operations and limitations

Messages that reach the configured receive threshold move to a DLQ and remain
there until an operator inspects them. This module does not automatically
redrive, delete, or replay DLQ messages. A future runbook/tool must inspect the
operation ID and payload, determine whether the command partially affected
resources, and use an idempotent or compensating action before replay. At-least-
once delivery still applies; DLQ presence is not proof that the operation had
no side effects.

The Control Plane stale-operation timeout is separate: it marks old pending or
running Operations failed but does not cancel an in-flight process. Schema v2
commands/results carry the Workspace revision; the Control Plane and Fake
Runtime reject stale revisions. A real Resource Plane Runtime still needs a
durable high-water mark and delete tombstone to fence side effects across
restarts; schema v1 legacy commands have no revision. The Resource Controller
rejects commands that have not started before `HAKO_COMMAND_MAX_AGE` (default
30 minutes) and reports `operation_expired`; configure this value no greater
than the Control Plane Operation timeout. This mitigates very old queued
messages, but does not stop already-running work or prove no partial side
effects. Cleanup of partially created AWS resources,
production runtime wiring, queue policies for DLQ inspection, and an audited
redrive process remain unimplemented.

## Monitoring and alert response

CloudWatch alarms are created for visible messages in each DLQ, oldest-message
age on each primary queue, and visible primary-queue backlog. Defaults are
300 seconds for queue age and 100 visible messages for backlog; the age alarm
requires two 60-second evaluation periods. Alarm names are exported by
`monitoring_alarm_names`. Set `alarm_actions` to an SNS topic ARN list if
notifications are required; the default is empty, so alarms are visible in
CloudWatch but do not notify anyone. When the fake Lambda is enabled, an
additional alarm detects Lambda invocation errors. There is not yet a
production Gateway/Controller health dashboard, throttling alarm, or
automated remediation.

When a DLQ alarm fires, first inspect the operation ID and payload, correlate
it with the DSQL operation history and controller logs, and check whether the
operation partially changed runtime resources. Do not redrive until the cause
is understood and retry safety or compensation is established.

## Inputs and outputs

The dev root is `infra/terraform/environments/dev/resource-plane`. Required
inputs are the Resource Plane identifier and exact Dispatcher/Result Consumer
role ARNs; queue, visibility, log retention, and additional tags have defaults.
Module outputs expose primary/DLQ URLs and ARNs, the Resource Controller role
ARN, its log-group name, and CloudWatch alarm names. The opt-in fake Lambda
requires the artifact from
`make build-resource-controller-lambda`; it is disabled by default. Do not put
AWS account credentials in Terraform variables or state.

The Lambda adapter's partial batch response, local Lambda MicroVM composition,
and fake-only Terraform deployment are documented in
[Resource Controller Lambda](resource-controller-lambda.md).

Validate without touching AWS resources:

```sh
tofu -chdir=infra/terraform/environments/dev/resource-plane init -backend=false -input=false
tofu -chdir=infra/terraform/environments/dev/resource-plane validate
```

No AWS `plan` or `apply` has been run as part of this implementation.
