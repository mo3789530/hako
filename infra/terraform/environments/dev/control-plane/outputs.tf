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
