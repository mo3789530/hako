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

output "resource_controller_role_arn" {
  value = module.resource_plane.resource_controller_role_arn
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

output "monitoring_alarm_names" {
  value = module.resource_plane.monitoring_alarm_names
}
