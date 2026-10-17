output "file_system_id" {
  description = "Encrypted EFS filesystem for Workspace Access Points."
  value       = aws_efs_file_system.workspace.id
}

output "file_system_arn" {
  description = "EFS filesystem ARN protected by the configured IAM/TLS policy."
  value       = aws_efs_file_system.workspace.arn
}

output "mount_target_security_group_id" {
  description = "Security group allowing only NFS from configured Lambda MicroVM connector groups."
  value       = aws_security_group.efs_mount_targets.id
}

output "mount_target_ids" {
  description = "EFS mount target IDs keyed by private subnet ID."
  value       = { for subnet_id, mount_target in aws_efs_mount_target.workspace : subnet_id => mount_target.id }
}

output "backup_vault_arn" {
  description = "Workspace EFS AWS Backup vault ARN."
  value       = aws_backup_vault.workspace.arn
}

output "backup_plan_id" {
  description = "Daily Workspace EFS backup plan ID."
  value       = aws_backup_plan.workspace.id
}
