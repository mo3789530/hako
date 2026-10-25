module "resource_plane" {
  source = "../../../modules/resource-plane"

  resource_plane_id                               = var.resource_plane_id
  control_plane_dispatcher_role_arn               = var.control_plane_dispatcher_role_arn
  control_plane_result_consumer_role_arn          = var.control_plane_result_consumer_role_arn
  control_plane_preview_result_consumer_role_arn  = var.control_plane_preview_result_consumer_role_arn
  control_plane_workload_result_consumer_role_arn = var.control_plane_workload_result_consumer_role_arn
  workload_controller_secret_arns                 = var.workload_controller_secret_arns
  workload_controller_runtime                     = var.workload_controller_runtime
  workload_artifact_storage                       = var.workload_artifact_storage
  workload_controller_vpc_config                  = var.workload_controller_vpc_config
  workload_controller_deployment                  = var.workload_controller_deployment
  enable_workload_result_queue                    = var.enable_workload_result_queue
  actions_runner_vpc_config                       = var.actions_runner_vpc_config
  actions_runner_deployment                       = var.actions_runner_deployment
  actions_runner_cleanup_deployment               = var.actions_runner_cleanup_deployment
  workspace_service_controller_vpc_config         = var.workspace_service_controller_vpc_config
  workspace_service_controller_deployment         = var.workspace_service_controller_deployment
  max_receive_count                               = var.max_receive_count
  command_visibility_timeout_seconds              = var.command_visibility_timeout_seconds
  result_visibility_timeout_seconds               = var.result_visibility_timeout_seconds
  message_retention_seconds                       = var.message_retention_seconds
  dlq_retention_seconds                           = var.dlq_retention_seconds
  log_retention_days                              = var.log_retention_days
  enable_fake_resource_controller                 = var.enable_fake_resource_controller
  fake_controller_lambda_zip_path                 = var.fake_controller_lambda_zip_path
  fake_controller_timeout_seconds                 = var.fake_controller_timeout_seconds
  fake_controller_execution_timeout_seconds       = var.fake_controller_execution_timeout_seconds
  fake_controller_memory_size                     = var.fake_controller_memory_size
  workspace_runtime_deployment                    = var.workspace_runtime_deployment
  enable_preview_runtime                          = var.enable_preview_runtime
  preview_runtime_source                          = var.preview_runtime_source
  tags                                            = var.tags
  alarm_actions                                   = var.alarm_actions
  oldest_message_age_alarm_seconds                = var.oldest_message_age_alarm_seconds
  queue_backlog_alarm_messages                    = var.queue_backlog_alarm_messages
  enable_health_report_alarms                     = var.enable_health_report_alarms
  health_report_secret_arn                        = var.health_report_secret_arn
  health_report_microvm_image_arn                 = var.health_report_microvm_image_arn
  health_report_microvm_image_version             = var.health_report_microvm_image_version
  health_report_vpc_egress_connector_arn          = var.health_report_vpc_egress_connector_arn
  health_report_controller_function_arn           = var.health_report_controller_function_arn
  health_report_workspace_efs_arn                 = var.health_report_workspace_efs_arn
  enable_health_reporter                          = var.enable_health_reporter
  health_reporter_lambda_zip_path                 = var.health_reporter_lambda_zip_path
  health_report_control_plane_endpoint            = var.health_report_control_plane_endpoint
  health_report_age_alarm_seconds                 = var.health_report_age_alarm_seconds
  health_report_unhealthy_runs_threshold          = var.health_report_unhealthy_runs_threshold
  workspace_storage_runtime = var.workspace_storage == null ? null : (
    var.workspace_storage.enable_dynamic_workspace_roles ? {
      file_system_id  = module.workspace_storage[0].file_system_id
      file_system_arn = module.workspace_storage[0].file_system_arn
    } : null
  )
  workspace_gateway_deployment         = var.workspace_gateway_deployment
  resource_plane_network_observability = var.resource_plane_network_observability
}

module "workspace_storage" {
  count  = var.workspace_storage == null ? 0 : 1
  source = "../../../modules/workspace-storage"

  resource_plane_id                    = var.resource_plane_id
  vpc_id                               = var.workspace_storage.vpc_id
  private_subnet_ids                   = var.workspace_storage.private_subnet_ids
  microvm_security_group_ids           = var.workspace_storage.microvm_security_group_ids
  workspace_access_point_role_bindings = var.workspace_storage.workspace_access_point_role_bindings
  enable_dynamic_workspace_roles       = var.workspace_storage.enable_dynamic_workspace_roles
  backup_schedule                      = var.workspace_storage.backup_schedule
  backup_retention_days                = var.workspace_storage.backup_retention_days
  tags                                 = var.tags
}
