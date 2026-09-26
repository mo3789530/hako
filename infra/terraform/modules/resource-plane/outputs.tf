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

output "resource_controller_role_arn" {
  description = "ARN of the least-privilege Resource Controller execution role."
  value       = aws_iam_role.resource_controller.arn
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
  description = "CloudWatch alarm names for SQS health and the optional Fake Resource Controller."
  value = concat(
    [
      aws_cloudwatch_metric_alarm.commands_dlq_visible.alarm_name,
      aws_cloudwatch_metric_alarm.results_dlq_visible.alarm_name,
      aws_cloudwatch_metric_alarm.commands_oldest_message.alarm_name,
      aws_cloudwatch_metric_alarm.results_oldest_message.alarm_name,
      aws_cloudwatch_metric_alarm.commands_backlog.alarm_name,
      aws_cloudwatch_metric_alarm.results_backlog.alarm_name,
    ],
    try([aws_cloudwatch_metric_alarm.fake_controller_errors[0].alarm_name], [])
  )
}
