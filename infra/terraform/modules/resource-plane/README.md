# Resource Plane messaging module

Creates encrypted SQS command/result queues and their DLQs, redrive and
redrive-allow policies, exact cross-account queue policies, a least-privilege
Resource Controller Lambda execution role, and a retained CloudWatch log
group. An explicit opt-in can create a development-only Fake Runtime Lambda
and SQS event source mapping. CloudWatch alarms cover DLQ messages, primary
queue age/backlog, and (when enabled) Fake Controller invocation errors;
notification actions are optional SNS topic ARNs. Production runtime,
VPC/network, and Gateway are not included.

See [`docs/resource-plane-queues-and-dlq.md`](../../../../docs/resource-plane-queues-and-dlq.md)
for queue semantics, IAM boundaries, operational limitations, and validation.

Required inputs: `resource_plane_id`,
`control_plane_dispatcher_role_arn`, and
`control_plane_result_consumer_role_arn`. All queue URLs/ARNs and the
controller role/log group and monitoring alarm names are exposed as outputs.

See [`docs/terraform-modules.md`](../../../../docs/terraform-modules.md) for the complete input/output inventory, resource tags, IAM boundaries, and dev environment differences.
