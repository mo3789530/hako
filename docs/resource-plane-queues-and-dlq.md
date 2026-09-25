# Resource Plane command/result queues and DLQ

The `infra/terraform/modules/resource-plane` module defines the initial AWS
message boundary for a Resource Plane. It is infrastructure groundwork only:
it does not deploy a Lambda function, event source mapping, controller binary,
Control Plane dispatcher/consumer, or any MicroVM resources. The development
root requires explicit Control Plane role ARNs and must be configured for the
intended Resource Plane AWS account before anyone runs `plan` or `apply`.

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
running Operations failed but does not cancel a queued/in-flight message or
fence late results. Runtime fencing, cleanup of partially created AWS
resources, DLQ alarm/metrics, event source mappings, queue policies for DLQ
inspection, and an audited redrive process remain unimplemented.

## Inputs and outputs

The dev root is `infra/terraform/environments/dev/resource-plane`. Required
inputs are the Resource Plane identifier and exact Dispatcher/Result Consumer
role ARNs; queue, visibility, log retention, and additional tags have defaults.
Module outputs expose primary/DLQ URLs and ARNs, the Resource Controller role
ARN, and its log-group name. Do not put AWS account credentials in Terraform
variables or state.

Validate without touching AWS resources:

```sh
tofu -chdir=infra/terraform/environments/dev/resource-plane init -backend=false -input=false
tofu -chdir=infra/terraform/environments/dev/resource-plane validate
```

No AWS `plan` or `apply` has been run as part of this implementation.
