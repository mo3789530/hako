# Resource Plane messaging module

Creates encrypted SQS command/Workspace Operation result queues and their
DLQs, redrive and redrive-allow policies, exact cross-account queue policies,
a least-privilege Resource Controller Lambda execution role, and a retained
CloudWatch log group. Separate Workload command and result queues/DLQs and a
Workload Controller role can be opted in with an exact Control Plane consumer
role ARN. A Worker Lambda remains disabled unless the separate
`workload_controller_deployment` object is provided.
Runtime IAM remains disabled unless `workload_controller_runtime` is configured
with concrete image, bucket/prefix, and lease ARNs plus five unique Secrets
Manager ARNs; only then are those exact Secrets readable and runtime actions
granted. Terraform never accepts Secret values. The manifest must pair the
Workload command/result queue URLs, and
the Dispatcher never falls back to the Workspace command queue. An explicit opt-in can create a development-only Fake Runtime Lambda
and SQS event source mapping. CloudWatch alarms cover DLQ messages, primary
queue age/backlog, and (when enabled) Fake Controller invocation errors.
Supplying `workload_controller_deployment` also creates the ARM64 combined
Workload Controller Lambda, a batch-size-1 partial-response mapping from only
the dedicated Workload command queue, a bounded EventBridge recovery schedule,
and Lambda/EventBridge invocation alarms. It requires an existing ZIP, HTTPS
Control Plane/broker endpoints, five exact Secret ARNs matching the IAM input,
the exact MicroVM/S3 runtime policy, bounded recovery settings, and at least
two reserved Lambda concurrency slots so a recovery invocation need not
consume the only command-processing slot. The object defaults to `null`; no Worker function, trigger, Secret access, or
MicroVM permissions are created by default. Secret values are never Terraform
inputs. Endpoint reachability, VPC/NAT/egress controls, bucket and KMS key
policies, image/git wiring, and live MicroVM isolation still need separate
review and validation. Alarm notification actions are optional SNS topic
ARNs; Gateway deployment is not included.

See [`docs/resource-plane-queues-and-dlq.md`](../../../../docs/resource-plane-queues-and-dlq.md)
for queue semantics, IAM boundaries, operational limitations, and validation.

Required inputs: `resource_plane_id`,
`control_plane_dispatcher_role_arn`, and
`control_plane_result_consumer_role_arn`. The runtime object and all five
Resource Plane-local Secret ARNs are optional and must be populated together
only after reviewing the worker's runtime policy. All queue URLs/ARNs and the
controller role/log group and monitoring alarm names are exposed as outputs;
Workload queue and role outputs are null unless enabled. The Worker Lambda,
SQS mapping, and recovery schedule outputs remain null unless deployment
configuration is provided.

See [`docs/terraform-modules.md`](../../../../docs/terraform-modules.md) for the complete input/output inventory, resource tags, IAM boundaries, and dev environment differences.

Run `make terraform-test` for AWS-mock plan tests that verify the Worker stays
disabled by default and validate the full opt-in Lambda/queue/recovery graph.
These tests neither apply resources nor contact AWS.
