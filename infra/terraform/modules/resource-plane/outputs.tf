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

output "monitoring_alarm_names" {
  description = "CloudWatch alarm names for Resource Plane queue, optional health reporter, and optional Fake Resource Controller monitoring."
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
    try([aws_cloudwatch_metric_alarm.health_report_stale[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_report_unhealthy[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_report_probe_failures[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.health_reporter_errors[0].alarm_name], []),
    try([aws_cloudwatch_metric_alarm.fake_controller_errors[0].alarm_name], [])
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
