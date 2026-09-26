output "http_api_id" {
  description = "ID of the Hako Control Plane HTTP API."
  value       = module.control_plane.http_api_id
}

output "http_api_endpoint" {
  description = "Invoke URL of the Hako Control Plane HTTP API."
  value       = module.control_plane.http_api_endpoint
}

output "cognito_jwt_authorizer_id" {
  description = "ID of the Cognito JWT authorizer for protected routes."
  value       = module.control_plane.cognito_jwt_authorizer_id
}

output "cognito_api_scope" {
  description = "OAuth scope clients must request and protected API routes must require."
  value       = module.control_plane.cognito_api_scope
}

output "cognito_app_client_id" {
  description = "ID of the Terraform-managed public Hako CLI app client."
  value       = module.control_plane.cognito_app_client_id
}

output "dsql_cluster_arn" {
  description = "Aurora DSQL cluster ARN used by the Control Plane."
  value       = module.control_plane.dsql_cluster_arn
}

output "dsql_endpoint" {
  description = "Aurora DSQL IAM-authenticated endpoint used by the API Lambda."
  value       = module.control_plane.dsql_endpoint
}

output "api_lambda_arn" {
  description = "ARN of the API Lambda currently targeted by API Gateway."
  value       = module.control_plane.api_lambda_arn
}

output "api_lambda_ecr_repository_url" {
  description = "Same-Region ECR repository for API Lambda release images."
  value       = module.control_plane.api_lambda_ecr_repository_url
}

output "api_lambda_image_candidate_arn" {
  description = "ARN of the prepared API image Lambda candidate, if configured."
  value       = module.control_plane.api_lambda_image_candidate_arn
}

output "api_lambda_role_arn" {
  description = "IAM role ARN mapped to the custom hako_api database role using AWS IAM GRANT."
  value       = module.control_plane.api_lambda_role_arn
}

output "dsql_migration_admin_policy_arn" {
  description = "Attach to a separate migration-only IAM principal; do not attach to the API Lambda role."
  value       = module.control_plane.dsql_migration_admin_policy_arn
}

output "outbox_dispatcher_role_arn" {
  value = module.control_plane.outbox_dispatcher_role_arn
}

output "outbox_dispatcher_lambda_arn" {
  value = module.control_plane.outbox_dispatcher_lambda_arn
}

output "outbox_dispatcher_schedule_arn" {
  value = module.control_plane.outbox_dispatcher_schedule_arn
}

output "outbox_dispatcher_error_alarm_name" {
  value = module.control_plane.outbox_dispatcher_error_alarm_name
}

output "github_webhook_processor_role_arn" {
  value = module.control_plane.github_webhook_processor_role_arn
}

output "github_webhook_processor_lambda_arn" {
  value = module.control_plane.github_webhook_processor_lambda_arn
}

output "github_webhook_processor_schedule_arn" {
  value = module.control_plane.github_webhook_processor_schedule_arn
}

output "github_webhook_processor_error_alarm_name" {
  value = module.control_plane.github_webhook_processor_error_alarm_name
}
