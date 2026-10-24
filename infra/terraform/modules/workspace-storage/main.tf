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
  dynamic_workspace_role_arn_pattern         = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/*"
  dynamic_workspace_access_point_arn_pattern = "arn:${data.aws_partition.current.partition}:elasticfilesystem:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:access-point/fsap-*"
  allowed_workspace_principal_arns = concat(
    [for binding in var.workspace_access_point_role_bindings : binding.role_arn],
    var.enable_dynamic_workspace_roles ? [local.dynamic_workspace_role_arn_pattern] : []
  )

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
    Statement = concat(
      [{
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Action    = "*"
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = "*" }
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
        }, {
        Sid       = "DenyUnconfiguredWorkspaceRuntimeRole"
        Effect    = "Deny"
        Action    = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = "*" }
        Condition = {
          ArnNotLike = {
            "aws:PrincipalArn" = local.allowed_workspace_principal_arns
          }
        }
      }],
      [for index, binding in var.workspace_access_point_role_bindings : {
        Sid       = "AllowWorkspaceAccessPoint${index}OverTLS"
        Effect    = "Allow"
        Action    = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = binding.role_arn }
        Condition = {
          Bool      = { "aws:SecureTransport" = "true" }
          ArnEquals = { "elasticfilesystem:AccessPointArn" = binding.access_point_arn }
        }
      }],
      [for index, binding in var.workspace_access_point_role_bindings : {
        Sid       = "DenyWorkspaceRoleOutsideAccessPoint${index}"
        Effect    = "Deny"
        Action    = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = binding.role_arn }
        Condition = {
          ArnNotEquals = { "elasticfilesystem:AccessPointArn" = binding.access_point_arn }
        }
      }],
      var.enable_dynamic_workspace_roles ? [{
        Sid       = "AllowDynamicWorkspaceRolesThroughEFSAccessPoints"
        Effect    = "Allow"
        Action    = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
        Resource  = aws_efs_file_system.workspace.arn
        Principal = { AWS = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root" }
        Condition = {
          Bool = { "aws:SecureTransport" = "true" }
          ArnLike = {
            "aws:PrincipalArn"                 = local.dynamic_workspace_role_arn_pattern
            "elasticfilesystem:AccessPointArn" = local.dynamic_workspace_access_point_arn_pattern
          }
        }
      }] : []
    )
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
