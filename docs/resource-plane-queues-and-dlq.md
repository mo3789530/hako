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
```

With `enable_fake_resource_controller = true`, the command queue can trigger
an optional development-only Lambda using batch size one and
`ReportBatchItemFailures`. A command is acknowledged only after its result has
been sent. This binary is hard-coded to `HAKO_RUNTIME_IMPLEMENTATION=fake`;
the in-memory runtime does not create or persist Workspace resources and is
not a production executor.

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
command queue, send on the result queue, and writes to its pre-created
CloudWatch log group. It has no permissions to create AWS runtime resources;
those permissions will be added only with a concrete runtime implementation.

The command queue resource policy trusts the exact configured Control Plane
Dispatcher role ARN for `sqs:SendMessage`. The result queue policy trusts the
exact Result Consumer role ARN for receive/delete/change-visibility/get-
attributes. The Control Plane roles also need matching identity policies in
their own account for cross-account SQS access. DLQs have no cross-account
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

The Lambda adapter's partial batch response and fake-only runtime constraints
are documented in [Resource Controller Lambda](resource-controller-lambda.md).

Validate without touching AWS resources:

```sh
tofu -chdir=infra/terraform/environments/dev/resource-plane init -backend=false -input=false
tofu -chdir=infra/terraform/environments/dev/resource-plane validate
```

No AWS `plan` or `apply` has been run as part of this implementation.
