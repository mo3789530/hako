output "http_api_id" {
  description = "ID of the Hako Control Plane HTTP API."
  value       = aws_apigatewayv2_api.control_plane.id
}

output "dsql_cluster_arn" {
  description = "Aurora DSQL cluster ARN."
  value       = aws_dsql_cluster.control_plane.arn
}

output "dsql_endpoint" {
  description = "Regional Aurora DSQL endpoint."
  value       = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
}

output "api_5xx_alarm_name" {
  description = "HTTP API server error alarm name."
  value       = aws_cloudwatch_metric_alarm.api_5xx.alarm_name
}

output "api_latency_p95_alarm_name" {
  description = "HTTP API p95 latency alarm name."
  value       = aws_cloudwatch_metric_alarm.api_latency_p95.alarm_name
}

output "dsql_query_timeouts_alarm_name" {
  description = "Aurora DSQL query timeout alarm name."
  value       = aws_cloudwatch_metric_alarm.dsql_query_timeouts.alarm_name
}

output "dsql_occ_conflicts_alarm_name" {
  description = "Aurora DSQL sustained OCC conflict alarm name."
  value       = aws_cloudwatch_metric_alarm.dsql_occ_conflicts.alarm_name
}

output "api_lambda_arn" {
  description = "ARN of the API Lambda currently targeted by API Gateway."
  value       = local.api_lambda_image_enabled ? aws_lambda_function.api_image[0].arn : aws_lambda_function.api.arn
}

output "api_lambda_ecr_repository_url" {
  description = "Same-Region immutable-tag ECR repository URL for API Lambda release images."
  value       = aws_ecr_repository.api_lambda.repository_url
}

output "api_lambda_image_candidate_arn" {
  description = "ARN of the prepared API image Lambda candidate; null when no image digest is configured."
  value       = try(aws_lambda_function.api_image[0].arn, null)
}

output "api_lambda_role_arn" {
  description = "IAM role ARN to associate to the configured custom DSQL API database role."
  value       = aws_iam_role.api_lambda.arn
}

output "dsql_migration_admin_policy_arn" {
  description = "IAM policy ARN granting admin database connection for migration/bootstrap work."
  value       = aws_iam_policy.dsql_migration_admin.arn
}

output "outbox_dispatcher_role_arn" {
  description = "IAM role ARN to allow in Resource Plane command queue policies and map to the hako_dispatcher DB role."
  value       = aws_iam_role.outbox_dispatcher.arn
}

output "outbox_dispatcher_lambda_arn" {
  description = "ARN of the optional scheduled Outbox Dispatcher Lambda; null unless enabled."
  value       = try(aws_lambda_function.outbox_dispatcher[0].arn, null)
}

output "outbox_dispatcher_schedule_arn" {
  description = "EventBridge schedule ARN for the optional Outbox Dispatcher; null unless enabled."
  value       = try(aws_cloudwatch_event_rule.outbox_dispatcher[0].arn, null)
}

output "outbox_dispatcher_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional Outbox Dispatcher; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.outbox_dispatcher_errors[0].alarm_name, null)
}

output "outbox_oldest_unpublished_age_alarm_name" {
  description = "CloudWatch alarm for the oldest unpublished Outbox event; null unless the Dispatcher is enabled."
  value       = try(aws_cloudwatch_metric_alarm.outbox_oldest_unpublished_age[0].alarm_name, null)
}

output "workload_scheduler_queue_url" {
  description = "URL of the durable Workload scheduler queue. A consumer is not included yet."
  value       = try(aws_sqs_queue.workload_scheduler[0].url, null)
}

output "workload_scheduler_dlq_url" {
  description = "URL of the Workload scheduler dead-letter queue."
  value       = try(aws_sqs_queue.workload_scheduler_dlq[0].url, null)
}

output "github_checks_queue_url" {
  description = "URL of the optional dedicated GitHub Checks delivery queue; null unless enabled."
  value       = try(aws_sqs_queue.github_checks[0].url, null)
}

output "github_checks_dlq_url" {
  description = "URL of the optional GitHub Checks dead-letter queue; null unless enabled."
  value       = try(aws_sqs_queue.github_checks_dlq[0].url, null)
}

output "github_checks_consumer_policy_arn" {
  description = "Least-privilege policy ARN for the dedicated GitHub Checks consumer runtime; attach only to that runtime's role."
  value       = try(aws_iam_policy.github_checks_consumer[0].arn, null)
}

output "github_checks_consumer_task_role_arn" {
  description = "Stable ECS task role ARN for the Checks consumer; queue/DSQL permissions attach only when Checks delivery is enabled."
  value       = aws_iam_role.github_checks_consumer.arn
}

output "github_checks_consumer_execution_role_arn" {
  description = "ECS task execution role; Secret-read permissions exist only when the Fargate consumer service is enabled."
  value       = try(aws_iam_role.github_checks_consumer_execution[0].arn, null)
}

output "github_checks_consumer_service_arn" {
  description = "ARN of the optional ECS/Fargate GitHub Checks consumer service."
  value       = try(aws_ecs_service.github_checks_consumer[0].id, null)
}

output "github_checks_consumer_task_definition_arn" {
  description = "ARN of the optional ARM64 GitHub Checks consumer task definition."
  value       = try(aws_ecs_task_definition.github_checks_consumer[0].arn, null)
}

output "github_checks_queue_age_alarm_name" {
  description = "Name of the GitHub Checks queue age alarm; null unless delivery is enabled."
  value       = try(aws_cloudwatch_metric_alarm.github_checks_queue_age[0].alarm_name, null)
}

output "github_checks_queue_depth_alarm_name" {
  description = "Name of the GitHub Checks queue depth alarm; null unless delivery is enabled."
  value       = try(aws_cloudwatch_metric_alarm.github_checks_queue_depth[0].alarm_name, null)
}

output "github_checks_dlq_alarm_name" {
  description = "Name of the GitHub Checks DLQ alarm; null unless delivery is enabled."
  value       = try(aws_cloudwatch_metric_alarm.github_checks_dlq_messages[0].alarm_name, null)
}

output "github_webhook_processor_role_arn" {
  description = "IAM role ARN to map to the least-privilege hako_webhook_processor DSQL user."
  value       = aws_iam_role.github_webhook_processor.arn
}

output "github_webhook_processor_lambda_arn" {
  description = "ARN of the optional scheduled GitHub Webhook Processor Lambda; null unless enabled."
  value       = try(aws_lambda_function.github_webhook_processor[0].arn, null)
}

output "github_webhook_processor_schedule_arn" {
  description = "EventBridge schedule ARN for the optional GitHub Webhook Processor; null unless enabled."
  value       = try(aws_cloudwatch_event_rule.github_webhook_processor[0].arn, null)
}

output "github_webhook_processor_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional processor; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.github_webhook_processor_errors[0].alarm_name, null)
}

output "workspace_idle_reconciler_role_arn" {
  description = "Stable IAM role ARN to map to the least-privilege hako_workspace_idle_reconciler DSQL user; its DbConnect policy is attached only when the schedule is enabled."
  value       = aws_iam_role.workspace_idle_reconciler.arn
}

output "workspace_idle_reconciler_lambda_arn" {
  description = "ARN of the optional scheduled Workspace idle reconciler Lambda; null unless enabled."
  value       = try(aws_lambda_function.workspace_idle_reconciler[0].arn, null)
}

output "workspace_idle_reconciler_schedule_arn" {
  description = "EventBridge schedule ARN for the optional Workspace idle reconciler; null unless enabled."
  value       = try(aws_cloudwatch_event_rule.workspace_idle_reconciler[0].arn, null)
}

output "workspace_idle_reconciler_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional Workspace idle reconciler; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.workspace_idle_reconciler_errors[0].alarm_name, null)
}

output "preview_expiry_reconciler_role_arn" {
  description = "Stable IAM role ARN to map to the least-privilege hako_preview_reconciler DSQL user; its DbConnect policy is attached only when enabled."
  value       = aws_iam_role.preview_expiry_reconciler.arn
}

output "preview_expiry_reconciler_lambda_arn" {
  description = "ARN of the optional scheduled Preview expiry reconciler Lambda; null unless enabled."
  value       = try(aws_lambda_function.preview_expiry_reconciler[0].arn, null)
}

output "preview_expiry_reconciler_schedule_arn" {
  description = "EventBridge schedule ARN for the optional Preview expiry reconciler; null unless enabled."
  value       = try(aws_cloudwatch_event_rule.preview_expiry_reconciler[0].arn, null)
}

output "preview_expiry_reconciler_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional Preview expiry reconciler; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.preview_expiry_reconciler_errors[0].alarm_name, null)
}

output "preview_runtime_dispatcher_role_arn" {
  description = "Stable IAM role ARN to map to the least-privilege hako_preview_runtime_dispatcher DSQL user."
  value       = aws_iam_role.preview_runtime_dispatcher.arn
}

output "preview_runtime_dispatcher_lambda_arn" {
  description = "ARN of the optional scheduled Preview command producer Lambda; null unless enabled."
  value       = try(aws_lambda_function.preview_runtime_dispatcher[0].arn, null)
}

output "preview_runtime_dispatcher_schedule_arn" {
  description = "EventBridge schedule ARN for the optional Preview command producer; null unless enabled."
  value       = try(aws_cloudwatch_event_rule.preview_runtime_dispatcher[0].arn, null)
}

output "preview_runtime_dispatcher_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional Preview command producer; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.preview_runtime_dispatcher_errors[0].alarm_name, null)
}

output "preview_result_consumer_role_arn" {
  description = "Stable IAM role ARN to map to the least-privilege hako_preview_result_consumer DSQL user and allow in Resource Plane result queue policies."
  value       = aws_iam_role.preview_result_consumer.arn
}

output "preview_result_consumer_lambda_arn" {
  description = "ARN of the optional SQS-triggered Preview result consumer Lambda; null unless enabled."
  value       = try(aws_lambda_function.preview_result_consumer[0].arn, null)
}

output "preview_result_consumer_event_source_mapping_uuids" {
  description = "SQS event-source mapping UUIDs keyed by Resource Plane ID."
  value       = { for plane_id, mapping in aws_lambda_event_source_mapping.preview_result_consumer : plane_id => mapping.uuid }
}

output "preview_result_consumer_error_alarm_name" {
  description = "CloudWatch invocation error alarm for the optional Preview result consumer; null unless enabled."
  value       = try(aws_cloudwatch_metric_alarm.preview_result_consumer_errors[0].alarm_name, null)
}

output "http_api_endpoint" {
  description = "Invoke URL of the Hako Control Plane HTTP API default stage."
  value       = aws_apigatewayv2_api.control_plane.api_endpoint
}

output "cognito_jwt_authorizer_id" {
  description = "ID of the Cognito JWT authorizer to attach to protected routes."
  value       = aws_apigatewayv2_authorizer.cognito.id
}

output "cognito_api_scope" {
  description = "OAuth scope clients must request and protected API routes must require."
  value       = aws_cognito_resource_server.hako_api.scope_identifiers[0]
}

output "cognito_app_client_id" {
  description = "ID of the Terraform-managed public Hako CLI app client."
  value       = aws_cognito_user_pool_client.cli.id
}
