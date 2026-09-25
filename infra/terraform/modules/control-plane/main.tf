terraform {
  required_version = ">= 1.12.1"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 6.0"
    }
  }
}

resource "aws_apigatewayv2_api" "control_plane" {
  name          = var.api_name
  protocol_type = "HTTP"

  tags = {
    "hako:managed-by" = "hako"
  }
}

resource "aws_apigatewayv2_authorizer" "cognito" {
  api_id           = aws_apigatewayv2_api.control_plane.id
  name             = "hako-cognito-jwt"
  authorizer_type  = "JWT"
  identity_sources = ["$request.header.Authorization"]

  jwt_configuration {
    audience = [aws_cognito_user_pool_client.cli.id]
    issuer   = var.cognito_issuer_url
  }
}

resource "aws_apigatewayv2_integration" "api_lambda" {
  api_id                 = aws_apigatewayv2_api.control_plane.id
  integration_type       = "AWS_PROXY"
  integration_method     = "POST"
  integration_uri        = var.api_lambda_arn
  payload_format_version = "2.0"
  timeout_milliseconds   = 29000
}

resource "aws_apigatewayv2_route" "api_lambda" {
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "$default"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "JWT"
  authorizer_id      = aws_apigatewayv2_authorizer.cognito.id
  authorization_scopes = [
    aws_cognito_resource_server.hako_api.scope_identifiers[0],
  ]
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.control_plane.id
  name        = "$default"
  auto_deploy = true
}

resource "aws_lambda_permission" "api_gateway" {
  statement_id  = "AllowHakoHTTPAPIInvoke"
  action        = "lambda:InvokeFunction"
  function_name = var.api_lambda_arn
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.control_plane.execution_arn}/*/*"
}

resource "aws_cognito_resource_server" "hako_api" {
  identifier   = "hako"
  name         = "Hako API"
  user_pool_id = var.cognito_user_pool_id

  scope {
    scope_name        = "api"
    scope_description = "Access Hako API endpoints."
  }
}

resource "aws_cognito_user_pool_client" "cli" {
  name         = var.cognito_cli_client_name
  user_pool_id = var.cognito_user_pool_id

  generate_secret                      = false
  enable_token_revocation              = true
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes = [
    "openid",
    "email",
    "profile",
    aws_cognito_resource_server.hako_api.scope_identifiers[0],
  ]
  callback_urls = var.cognito_cli_callback_urls
  supported_identity_providers = [
    "COGNITO",
  ]
}
