module "resource_plane" {
  source = "../../../modules/resource-plane"

  resource_plane_id                         = var.resource_plane_id
  control_plane_dispatcher_role_arn         = var.control_plane_dispatcher_role_arn
  control_plane_result_consumer_role_arn    = var.control_plane_result_consumer_role_arn
  max_receive_count                         = var.max_receive_count
  command_visibility_timeout_seconds        = var.command_visibility_timeout_seconds
  result_visibility_timeout_seconds         = var.result_visibility_timeout_seconds
  message_retention_seconds                 = var.message_retention_seconds
  dlq_retention_seconds                     = var.dlq_retention_seconds
  log_retention_days                        = var.log_retention_days
  enable_fake_resource_controller           = var.enable_fake_resource_controller
  fake_controller_lambda_zip_path           = var.fake_controller_lambda_zip_path
  fake_controller_timeout_seconds           = var.fake_controller_timeout_seconds
  fake_controller_execution_timeout_seconds = var.fake_controller_execution_timeout_seconds
  fake_controller_memory_size               = var.fake_controller_memory_size
  tags                                      = var.tags
  alarm_actions                             = var.alarm_actions
  oldest_message_age_alarm_seconds          = var.oldest_message_age_alarm_seconds
  queue_backlog_alarm_messages              = var.queue_backlog_alarm_messages
}
