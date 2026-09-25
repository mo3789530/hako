module "control_plane" {
  source = "../../../modules/control-plane"

  cognito_issuer_url        = var.cognito_issuer_url
  cognito_user_pool_id      = var.cognito_user_pool_id
  cognito_cli_callback_urls = var.cognito_cli_callback_urls
  api_lambda_arn            = var.api_lambda_arn
}
