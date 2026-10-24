output "commands_queue_url" {
  description = "URL of the Resource Plane command queue."
  value       = aws_sqs_queue.commands.url
}

output "commands_queue_arn" {
  description = "ARN of the Resource Plane command queue."
  value       = aws_sqs_queue.commands.arn
}

output "commands_dlq_url" {
  description = "URL of the Resource Plane command dead-letter queue."
  value       = aws_sqs_queue.commands_dlq.url
}

output "commands_dlq_arn" {
  description = "ARN of the Resource Plane command dead-letter queue."
  value       = aws_sqs_queue.commands_dlq.arn
}

output "results_queue_url" {
  description = "URL of the Resource Plane result queue."
  value       = aws_sqs_queue.results.url
}

output "results_queue_arn" {
  description = "ARN of the Resource Plane result queue."
  value       = aws_sqs_queue.results.arn
}

output "results_dlq_url" {
  description = "URL of the Resource Plane result dead-letter queue."
  value       = aws_sqs_queue.results_dlq.url
}

output "results_dlq_arn" {
  description = "ARN of the Resource Plane result dead-letter queue."
  value       = aws_sqs_queue.results_dlq.arn
}

output "preview_runtime_commands_queue_url" {
  description = "URL of the dedicated Preview runtime command queue; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_commands[0].url, null)
}

output "preview_runtime_commands_queue_arn" {
  description = "ARN of the dedicated Preview runtime command queue; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_commands[0].arn, null)
}

output "preview_runtime_results_queue_url" {
  description = "URL of the dedicated Preview runtime result queue; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_results[0].url, null)
}

output "preview_runtime_results_queue_arn" {
  description = "ARN of the dedicated Preview runtime result queue; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_results[0].arn, null)
}

output "preview_runtime_commands_dlq_url" {
  description = "URL of the Preview runtime command DLQ; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_commands_dlq[0].url, null)
}

output "preview_runtime_commands_dlq_arn" {
  description = "ARN of the Preview runtime command DLQ; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_commands_dlq[0].arn, null)
}

output "preview_runtime_results_dlq_url" {
  description = "URL of the Preview runtime result DLQ; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_results_dlq[0].url, null)
}

output "preview_runtime_results_dlq_arn" {
  description = "ARN of the Preview runtime result DLQ; null unless Preview runtime deployment is configured."
  value       = try(aws_sqs_queue.preview_runtime_results_dlq[0].arn, null)
}

output "preview_runtime_controller_event_source_mapping_uuid" {
  description = "SQS event source mapping for optional Preview runtime commands."
  value       = try(aws_lambda_event_source_mapping.preview_runtime_controller[0].uuid, null)
}

output "workload_results_queue_url" {
  description = "URL of the dedicated Workload Run result queue; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_results[0].url, null)
}

output "workload_results_queue_arn" {
  description = "ARN of the dedicated Workload Run result queue; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_results[0].arn, null)
}

output "workload_results_dlq_url" {
  description = "URL of the dedicated Workload Run result DLQ; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_results_dlq[0].url, null)
}

output "workload_results_dlq_arn" {
  description = "ARN of the dedicated Workload Run result DLQ; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_results_dlq[0].arn, null)
}

output "workload_commands_queue_url" {
  description = "URL of the dedicated Workload command queue; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_commands[0].url, null)
}

output "workload_commands_queue_arn" {
  description = "ARN of the dedicated Workload command queue; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_commands[0].arn, null)
}

output "workload_commands_dlq_url" {
  description = "URL of the dedicated Workload command DLQ; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_commands_dlq[0].url, null)
}

output "workload_commands_dlq_arn" {
  description = "ARN of the dedicated Workload command DLQ; null unless explicitly enabled."
  value       = try(aws_sqs_queue.workload_commands_dlq[0].arn, null)
}

output "actions_runner_startup_queue_url" {
  description = "URL of the opt-in Actions Runner startup queue; null until its worker is configured."
  value       = try(aws_sqs_queue.actions_runner_startup[0].url, null)
}

output "actions_runner_startup_queue_arn" {
  description = "ARN of the opt-in Actions Runner startup queue; null until its worker is configured."
  value       = try(aws_sqs_queue.actions_runner_startup[0].arn, null)
}

output "actions_runner_startup_dlq_arn" {
  description = "ARN of the opt-in Actions Runner startup DLQ; null until its worker is configured."
  value       = try(aws_sqs_queue.actions_runner_startup_dlq[0].arn, null)
}

output "actions_runner_worker_lambda_arn" {
  description = "ARN of the opt-in Lambda MicroVM Actions Runner worker."
  value       = try(aws_lambda_function.actions_runner_worker[0].arn, null)
}

output "actions_runner_worker_event_source_mapping_uuid" {
  description = "SQS event-source mapping for the Actions Runner startup worker."
  value       = try(aws_lambda_event_source_mapping.actions_runner_worker[0].uuid, null)
}

output "actions_runner_cleanup_lambda_arn" {
  description = "ARN of the separately opted-in scheduled Actions Runner cleanup Lambda."
  value       = try(aws_lambda_function.actions_runner_cleanup[0].arn, null)
}

output "actions_runner_cleanup_schedule_arn" {
  description = "EventBridge schedule ARN for Actions Runner cleanup; null unless explicitly enabled."
  value       = try(aws_cloudwatch_event_rule.actions_runner_cleanup[0].arn, null)
}

output "actions_runner_cleanup_errors_alarm_name" {
  description = "CloudWatch alarm for scheduled Actions Runner cleanup invocation errors."
  value       = try(aws_cloudwatch_metric_alarm.actions_runner_cleanup_errors[0].alarm_name, null)
}

output "workspace_service_commands_queue_url" {
  description = "URL of the opt-in Workspace Service command queue; null until its consumer is configured."
  value       = try(aws_sqs_queue.workspace_service_commands[0].url, null)
}

output "workspace_service_commands_queue_arn" {
  description = "ARN of the opt-in Workspace Service command queue; null until its consumer is configured."
  value       = try(aws_sqs_queue.workspace_service_commands[0].arn, null)
}

output "workspace_service_commands_dlq_arn" {
  description = "ARN of the opt-in Workspace Service dead-letter queue; null until its consumer is configured."
  value       = try(aws_sqs_queue.workspace_service_commands_dlq[0].arn, null)
}

output "workspace_service_controller_lambda_arn" {
  description = "ARN of the opt-in Workspace Service Lambda MicroVM consumer."
  value       = try(aws_lambda_function.workspace_service_controller[0].arn, null)
}

output "workspace_service_controller_event_source_mapping_uuid" {
  description = "SQS event-source mapping UUID for the opt-in Workspace Service consumer."
  value       = try(aws_lambda_event_source_mapping.workspace_service_controller[0].uuid, null)
}

output "workspace_service_controller_role_arn" {
  description = "Least-privilege Workspace Service consumer role; null unless explicitly deployed."
  value       = try(aws_iam_role.workspace_service_controller[0].arn, null)
}

output "workspace_service_controller_log_group_name" {
  description = "Retained Workspace Service consumer log group; null unless explicitly deployed."
  value       = try(aws_cloudwatch_log_group.workspace_service_controller[0].name, null)
}

output "resource_controller_role_arn" {
  description = "ARN of the least-privilege Resource Controller execution role."
  value       = aws_iam_role.resource_controller.arn
}

output "workload_controller_role_arn" {
  description = "Minimal Workload Controller role; it can read only configured exact Secrets Manager ARNs and is null unless the dedicated Workload queue is enabled."
  value       = try(aws_iam_role.workload_controller[0].arn, null)
}

output "workload_controller_log_group_name" {
  description = "Pre-created Workload Controller log group; null unless the dedicated Workload result queue is enabled."
  value       = try(aws_cloudwatch_log_group.workload_controller[0].name, null)
}

output "workload_controller_lambda_arn" {
  description = "ARN of the opt-in combined Workload command/recovery Lambda; null until deployment configuration is supplied."
  value       = try(aws_lambda_function.workload_controller[0].arn, null)
}

output "workload_controller_event_source_mapping_uuid" {
  description = "SQS event source mapping UUID for the opt-in Workload Controller; null unless deployed."
  value       = try(aws_lambda_event_source_mapping.workload_controller[0].uuid, null)
}

output "workload_recovery_schedule_arn" {
  description = "EventBridge schedule ARN for the opt-in bounded Workload recovery sweep; null unless deployed."
  value       = try(aws_cloudwatch_event_rule.workload_controller_recovery[0].arn, null)
}

output "workload_artifact_cleanup_schedule_arn" {
  description = "EventBridge schedule ARN for bounded Workload Artifact cleanup; null unless deployed."
  value       = try(aws_cloudwatch_event_rule.workload_artifact_cleanup[0].arn, null)
}

output "workload_artifact_bucket_arn" {
  description = "ARN of the optional private, versioned Workload artifact bucket; null when bucket provisioning is disabled."
  value       = try(aws_s3_bucket.workload_artifacts[0].arn, null)
}

output "workload_artifact_bucket_name" {
  description = "Name of the optional private Workload artifact bucket; null when bucket provisioning is disabled."
  value       = try(aws_s3_bucket.workload_artifacts[0].bucket, null)
}

output "resource_controller_log_group_name" {
  description = "Pre-created CloudWatch Log Group for a future Resource Controller Lambda."
  value       = aws_cloudwatch_log_group.resource_controller.name
}

output "fake_resource_controller_lambda_arn" {
  description = "ARN of the optional development-only Fake Runtime Lambda; null unless explicitly enabled."
  value       = try(aws_lambda_function.fake_resource_controller[0].arn, null)
}

output "fake_resource_controller_event_source_mapping_uuid" {
  description = "SQS event source mapping UUID for the optional fake controller."
  value       = try(aws_lambda_event_source_mapping.fake_resource_controller[0].uuid, null)
}

output "fake_resource_controller_log_group_name" {
  description = "Pre-created CloudWatch Log Group for the optional fake Lambda."
  value       = try(aws_cloudwatch_log_group.fake_resource_controller[0].name, null)
}

output "workspace_resource_controller_lambda_arn" {
  description = "ARN of the optional default-off Lambda MicroVM Workspace Resource Controller; null unless a reviewed workspace_runtime_deployment is configured."
  value       = try(aws_lambda_function.workspace_resource_controller[0].arn, null)
}

output "workspace_resource_controller_event_source_mapping_uuid" {
  description = "SQS event source mapping UUID for the optional Lambda MicroVM Workspace Resource Controller."
  value       = try(aws_lambda_event_source_mapping.workspace_resource_controller[0].uuid, null)
}

output "monitoring_alarm_names" {
  description = "CloudWatch alarm names for Resource Plane queues and optional health, Fake, Workspace MicroVM, Preview runtime, Workload, Service, and Gateway controllers."
  value = concat(
    [
      aws_cloudwatch_metric_alarm.commands_dlq_visible.alarm_name,
      aws_cloudwatch_metric_alarm.results_dlq_visible.alarm_name,
      aws_cloudwatch_metric_alarm.commands_oldest_message.alarm_name,
      aws_cloudwatch_metric_alarm.results_oldest_message.alarm_name,
      aws_cloudwatch_metric_alarm.commands_backlog.alarm_name,
      aws_cloudwatch_metric_alarm.results_backlog.alarm_name,
    ],
    try([aws_cloudwatch_metric_alarm.workload_results_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_results_oldest_message[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_results_backlog[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_commands_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_commands_oldest_message[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_commands_backlog[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_controller_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_recovery_failed_invocations[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workload_artifact_cleanup_failed_invocations[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.actions_runner_startup_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_service_commands_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_service_commands_oldest_message[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_service_controller_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_report_stale[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_report_unhealthy[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_report_probe_failures[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_reporter_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.fake_controller_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_resource_controller_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.preview_runtime_commands_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.preview_runtime_commands_oldest_message[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.preview_runtime_results_dlq_visible[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.preview_runtime_results_oldest_message[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_gateway_unhealthy_targets[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.workspace_gateway_alb_5xx[0].alarm_name], [])
  )
}

output "health_reporter_log_group_name" {
  description = "Retention-limited CloudWatch Log Group for the optional Resource Plane health reporter; null unless reporter or health alarms are enabled."
  value       = try(aws_cloudwatch_log_group.health_reporter[0].name, null)
}

output "health_reporter_role_arn" {
  description = "Dedicated opt-in Lambda role limited to the exact HMAC Secret, two queue metadata APIs, configured MicroVM image version and VPC egress connector, and reporter log group; null unless the reporter is enabled."
  value       = try(aws_iam_role.health_reporter[0].arn, null)
}

output "workspace_gateway_url" {
  description = "Configured public HTTPS/WSS origin of the optional Workspace Gateway; null until explicitly deployed."
  value       = try(var.workspace_gateway_deployment.public_origin, null)
}

output "workspace_gateway_alb_dns_name" {
  description = "AWS-assigned DNS name of the optional Workspace Gateway ALB; configure public_origin DNS to point here."
  value       = try(aws_lb.workspace_gateway[0].dns_name, null)
}

output "workspace_gateway_service_arn" {
  description = "ARN of the optional private ECS/Fargate Workspace Gateway service."
  value       = try(aws_ecs_service.workspace_gateway[0].id, null)
}

output "workspace_gateway_task_role_arn" {
  description = "Task role ARN for the optional Workspace Gateway exact runtime and Secret allowlists."
  value       = try(aws_iam_role.workspace_gateway_task[0].arn, null)
}

output "workspace_gateway_alb_arn" {
  description = "ARN of the optional HTTPS/WSS-only Workspace Gateway ALB."
  value       = try(aws_lb.workspace_gateway[0].arn, null)
}

output "workspace_gateway_alarm_names" {
  description = "Health and ALB error alarms for the optional Workspace Gateway; empty when disabled."
  value = var.workspace_gateway_deployment == null ? [] : [
    aws_cloudwatch_metric_alarm.workspace_gateway_unhealthy_targets[0].alarm_name,
    aws_cloudwatch_metric_alarm.workspace_gateway_alb_5xx[0].alarm_name,
  ]
}
