# Resource Plane messaging module

Creates encrypted SQS command/result queues and their DLQs, redrive and
redrive-allow policies, exact cross-account queue policies, a least-privilege
Resource Controller Lambda execution role, and a retained CloudWatch log
group. It deliberately does not create the Lambda function, event source
mapping, VPC/network, Gateway, or runtime resource permissions.

See [`docs/resource-plane-queues-and-dlq.md`](../../../../docs/resource-plane-queues-and-dlq.md)
for queue semantics, IAM boundaries, operational limitations, and validation.

Required inputs: `resource_plane_id`,
`control_plane_dispatcher_role_arn`, and
`control_plane_result_consumer_role_arn`. All queue URLs/ARNs and the
controller role/log group are exposed as outputs.
