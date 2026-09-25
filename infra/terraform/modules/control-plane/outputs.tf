output "http_api_id" {
  description = "ID of the Hako Control Plane HTTP API."
  value       = aws_apigatewayv2_api.control_plane.id
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
