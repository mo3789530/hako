module "resource_plane" {
  source = "../../../modules/resource-plane"

  resource_plane_id                      = var.resource_plane_id
  control_plane_dispatcher_role_arn      = var.control_plane_dispatcher_role_arn
  control_plane_result_consumer_role_arn = var.control_plane_result_consumer_role_arn
  max_receive_count                      = var.max_receive_count
  command_visibility_timeout_seconds     = var.command_visibility_timeout_seconds
  result_visibility_timeout_seconds      = var.result_visibility_timeout_seconds
  message_retention_seconds              = var.message_retention_seconds
  dlq_retention_seconds                  = var.dlq_retention_seconds
  log_retention_days                     = var.log_retention_days
  tags                                   = var.tags
}
