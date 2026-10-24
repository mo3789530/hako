output "commands_queue_url" {
  value = module.resource_plane.commands_queue_url
}

output "commands_queue_arn" {
  value = module.resource_plane.commands_queue_arn
}

output "commands_dlq_url" {
  value = module.resource_plane.commands_dlq_url
}

output "commands_dlq_arn" {
  value = module.resource_plane.commands_dlq_arn
}

output "results_queue_url" {
  value = module.resource_plane.results_queue_url
}

output "results_queue_arn" {
  value = module.resource_plane.results_queue_arn
}

output "results_dlq_url" {
  value = module.resource_plane.results_dlq_url
}

output "results_dlq_arn" {
  value = module.resource_plane.results_dlq_arn
}

output "workload_results_queue_url" {
  value = module.resource_plane.workload_results_queue_url
}

output "workload_results_queue_arn" {
  value = module.resource_plane.workload_results_queue_arn
}

output "workload_results_dlq_url" {
  value = module.resource_plane.workload_results_dlq_url
}

output "workload_results_dlq_arn" {
  value = module.resource_plane.workload_results_dlq_arn
}

output "workload_commands_queue_url" {
  value = module.resource_plane.workload_commands_queue_url
}

output "workload_commands_queue_arn" {
  value = module.resource_plane.workload_commands_queue_arn
}

output "workload_commands_dlq_url" {
  value = module.resource_plane.workload_commands_dlq_url
}

output "workload_commands_dlq_arn" {
  value = module.resource_plane.workload_commands_dlq_arn
}

output "actions_runner_startup_queue_url" {
  value = module.resource_plane.actions_runner_startup_queue_url
}

output "actions_runner_startup_queue_arn" {
  value = module.resource_plane.actions_runner_startup_queue_arn
}

output "actions_runner_startup_dlq_arn" {
  value = module.resource_plane.actions_runner_startup_dlq_arn
}

output "actions_runner_worker_lambda_arn" {
  value = module.resource_plane.actions_runner_worker_lambda_arn
}

output "actions_runner_worker_event_source_mapping_uuid" {
  value = module.resource_plane.actions_runner_worker_event_source_mapping_uuid
}

output "actions_runner_cleanup_lambda_arn" {
  value = module.resource_plane.actions_runner_cleanup_lambda_arn
}

output "actions_runner_cleanup_schedule_arn" {
  value = module.resource_plane.actions_runner_cleanup_schedule_arn
}

output "actions_runner_cleanup_errors_alarm_name" {
  value = module.resource_plane.actions_runner_cleanup_errors_alarm_name
}

output "workspace_service_commands_queue_url" {
  value = module.resource_plane.workspace_service_commands_queue_url
}

output "workspace_service_commands_queue_arn" {
  value = module.resource_plane.workspace_service_commands_queue_arn
}

output "workspace_service_commands_dlq_arn" {
  value = module.resource_plane.workspace_service_commands_dlq_arn
}

output "workspace_service_controller_lambda_arn" {
  value = module.resource_plane.workspace_service_controller_lambda_arn
}

output "workspace_service_controller_event_source_mapping_uuid" {
  value = module.resource_plane.workspace_service_controller_event_source_mapping_uuid
}

output "workspace_service_controller_role_arn" {
  value = module.resource_plane.workspace_service_controller_role_arn
}

output "workspace_service_controller_log_group_name" {
  value = module.resource_plane.workspace_service_controller_log_group_name
}

output "workload_controller_lambda_arn" {
  value = module.resource_plane.workload_controller_lambda_arn
}

output "workload_controller_event_source_mapping_uuid" {
  value = module.resource_plane.workload_controller_event_source_mapping_uuid
}

output "workload_recovery_schedule_arn" {
  value = module.resource_plane.workload_recovery_schedule_arn
}

output "workload_artifact_cleanup_schedule_arn" {
  value = module.resource_plane.workload_artifact_cleanup_schedule_arn
}

output "workload_artifact_bucket_arn" {
  description = "Private Workload artifact bucket ARN; null when managed artifact storage is disabled."
  value       = module.resource_plane.workload_artifact_bucket_arn
}

output "workload_artifact_bucket_name" {
  description = "Private Workload artifact bucket name; null when managed artifact storage is disabled."
  value       = module.resource_plane.workload_artifact_bucket_name
}

output "resource_controller_role_arn" {
  value = module.resource_plane.resource_controller_role_arn
}

output "workload_controller_role_arn" {
  value = module.resource_plane.workload_controller_role_arn
}

output "workload_controller_log_group_name" {
  value = module.resource_plane.workload_controller_log_group_name
}

output "resource_controller_log_group_name" {
  value = module.resource_plane.resource_controller_log_group_name
}

output "fake_resource_controller_lambda_arn" {
  value = module.resource_plane.fake_resource_controller_lambda_arn
}

output "fake_resource_controller_event_source_mapping_uuid" {
  value = module.resource_plane.fake_resource_controller_event_source_mapping_uuid
}

output "fake_resource_controller_log_group_name" {
  value = module.resource_plane.fake_resource_controller_log_group_name
}

output "workspace_resource_controller_lambda_arn" {
  value = module.resource_plane.workspace_resource_controller_lambda_arn
}

output "workspace_resource_controller_event_source_mapping_uuid" {
  value = module.resource_plane.workspace_resource_controller_event_source_mapping_uuid
}

output "monitoring_alarm_names" {
  value = module.resource_plane.monitoring_alarm_names
}

output "workspace_storage_file_system_id" {
  description = "Encrypted Workspace EFS filesystem ID when optional storage is configured; otherwise null."
  value       = try(module.workspace_storage[0].file_system_id, null)
}

output "workspace_storage_mount_target_security_group_id" {
  description = "EFS mount-target security group ID when optional storage is configured; otherwise null."
  value       = try(module.workspace_storage[0].mount_target_security_group_id, null)
}

output "workspace_gateway_url" {
  value = module.resource_plane.workspace_gateway_url
}

output "workspace_gateway_alb_dns_name" {
  value = module.resource_plane.workspace_gateway_alb_dns_name
}

output "workspace_gateway_service_arn" {
  value = module.resource_plane.workspace_gateway_service_arn
}

output "workspace_gateway_alb_arn" {
  value = module.resource_plane.workspace_gateway_alb_arn
}

output "workspace_gateway_task_role_arn" {
  value = module.resource_plane.workspace_gateway_task_role_arn
}

output "workspace_gateway_alarm_names" {
  value = module.resource_plane.workspace_gateway_alarm_names
}
