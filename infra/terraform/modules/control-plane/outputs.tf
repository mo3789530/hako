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
