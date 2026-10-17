terraform {
  required_version = ">= 1.12.1"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 6.0"
    }
  }
}

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  common_tags = merge(var.tags, {
    "hako:managed-by"        = "hako"
    "hako:resource-plane-id" = var.resource_plane_id
    "hako:purpose"           = "workspace-storage"
  })

  backup_assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "backup.amazonaws.com" }
    }]
  })

  filesystem_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Action    = "*"
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = "*" }
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
      {
        Sid       = "AllowConfiguredRuntimeRoleOverTLS"
        Effect    = "Allow"
        Action    = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = var.workspace_runtime_role_arns }
        Condition = {
          Bool = { "aws:SecureTransport" = "true" }
          StringLike = {
            "elasticfilesystem:AccessPointArn" = ["arn:${data.aws_partition.current.partition}:elasticfilesystem:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:access-point/*"]
          }
        }
      }
    ]
  })
}

resource "aws_efs_file_system" "workspace" {
  creation_token = "${var.resource_plane_id}-workspace"
  encrypted      = true

  performance_mode = "generalPurpose"
  throughput_mode  = "bursting"

  lifecycle_policy {
    transition_to_ia = "AFTER_30_DAYS"
  }

  tags = local.common_tags
}

resource "aws_security_group" "efs_mount_targets" {
  name_prefix = "${var.resource_plane_id}-workspace-efs-"
  description = "Private NFS ingress from approved Hako Lambda MicroVM connector security groups only."
  vpc_id      = var.vpc_id

  ingress = []
  egress  = []

  tags = local.common_tags
}

resource "aws_vpc_security_group_ingress_rule" "efs_from_microvm" {
  for_each = toset(var.microvm_security_group_ids)

  security_group_id            = aws_security_group.efs_mount_targets.id
  referenced_security_group_id = each.value
  description                  = "NFS from approved Lambda MicroVM connector"
  ip_protocol                  = "tcp"
  from_port                    = 2049
  to_port                      = 2049
}

resource "aws_efs_mount_target" "workspace" {
  for_each = toset(var.private_subnet_ids)

  file_system_id  = aws_efs_file_system.workspace.id
  subnet_id       = each.value
  security_groups = [aws_security_group.efs_mount_targets.id]
}


resource "aws_iam_role" "backup" {
  name_prefix        = "${var.resource_plane_id}-workspace-backup-"
  assume_role_policy = local.backup_assume_role_policy
  tags               = local.common_tags
}

resource "aws_iam_role_policy_attachment" "backup" {
  role       = aws_iam_role.backup.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup"
}

resource "aws_backup_vault" "workspace" {
  name = "${var.resource_plane_id}-workspace"
  tags = local.common_tags
}

resource "aws_backup_plan" "workspace" {
  name = "${var.resource_plane_id}-workspace-daily"
  tags = local.common_tags

  rule {
    rule_name                = "daily-workspace-filesystem"
    target_vault_name        = aws_backup_vault.workspace.name
    schedule                 = var.backup_schedule
    start_window             = 60
    completion_window        = 360
    enable_continuous_backup = false

    lifecycle {
      delete_after = var.backup_retention_days
    }
  }
}

resource "aws_backup_selection" "workspace" {
  name         = "${var.resource_plane_id}-workspace-filesystem"
  iam_role_arn = aws_iam_role.backup.arn
  plan_id      = aws_backup_plan.workspace.id
  resources    = [aws_efs_file_system.workspace.arn]
}

resource "aws_efs_file_system_policy" "workspace" {
  file_system_id = aws_efs_file_system.workspace.id
  policy         = local.filesystem_policy
}
