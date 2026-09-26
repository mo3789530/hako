module "control_plane" {
  source = "../../../modules/control-plane"

  aws_region                               = var.aws_region
  cognito_issuer_url                       = var.cognito_issuer_url
  cognito_user_pool_id                     = var.cognito_user_pool_id
  cognito_cli_callback_urls                = var.cognito_cli_callback_urls
  api_lambda_zip_path                      = var.api_lambda_zip_path
  github_webhook_secret_arn                = var.github_webhook_secret_arn
  github_app_slug                          = var.github_app_slug
  github_app_id                            = var.github_app_id
  github_app_client_id                     = var.github_app_client_id
  github_app_client_secret_arn             = var.github_app_client_secret_arn
  github_app_callback_url                  = var.github_app_callback_url
  github_app_setup_enabled                 = var.github_app_setup_enabled
  api_lambda_image_uri                     = var.api_lambda_image_uri
  api_lambda_image_active                  = var.api_lambda_image_active
  default_workspace_image                  = var.default_workspace_image
  tags                                     = var.tags
  enable_outbox_dispatcher                 = var.enable_outbox_dispatcher
  dispatcher_lambda_zip_path               = var.dispatcher_lambda_zip_path
  enable_github_webhook_processor          = var.enable_github_webhook_processor
  github_webhook_processor_lambda_zip_path = var.github_webhook_processor_lambda_zip_path
  resource_plane_manifest_json             = var.resource_plane_manifest_json
}
