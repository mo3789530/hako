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
