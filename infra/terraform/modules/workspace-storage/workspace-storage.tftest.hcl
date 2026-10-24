mock_provider "aws" {
  alias = "mock"

  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { name = "ap-northeast-1" }
  }
  mock_resource "aws_efs_file_system" {
    defaults = {
      id  = "fs-0123456789abcdef0"
      arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:file-system/fs-0123456789abcdef0"
    }
  }
  mock_resource "aws_security_group" {
    defaults = { id = "sg-0123456789abcdef0" }
  }
  mock_resource "aws_vpc_security_group_ingress_rule" {
    defaults = { id = "sgr-0123456789abcdef0" }
  }
  mock_resource "aws_iam_role" {
    defaults = {
      id  = "rp-test-workspace-backup"
      arn = "arn:aws:iam::123456789012:role/rp-test-workspace-backup"
    }
  }
  mock_resource "aws_backup_vault" {
    defaults = { arn = "arn:aws:backup:ap-northeast-1:123456789012:backup-vault:rp-test-workspace" }
  }
  mock_resource "aws_backup_plan" {
    defaults = { id = "plan-workspace" }
  }
  mock_resource "aws_efs_mount_target" {
    defaults = { id = "fsmt-0123456789abcdef0" }
  }
}

run "workspace_storage_is_encrypted_private_and_backed_up" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id          = "rp-test"
    vpc_id                     = "vpc-0123456789abcdef0"
    private_subnet_ids         = ["subnet-11111111", "subnet-22222222"]
    microvm_security_group_ids = ["sg-11111111", "sg-22222222"]
    workspace_access_point_role_bindings = [{
      role_arn         = "arn:aws:iam::123456789012:role/hako-ws-runtime-a"
      access_point_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"
    }]
  }

  assert {
    condition     = aws_efs_file_system.workspace.encrypted && aws_efs_file_system.workspace.throughput_mode == "bursting"
    error_message = "Workspace storage must have encryption at rest and a bounded known throughput mode."
  }

  assert {
    condition     = length(aws_efs_mount_target.workspace) == 2 && length(aws_vpc_security_group_ingress_rule.efs_from_microvm) == 2 && length(aws_security_group.efs_mount_targets.egress) == 0
    error_message = "Mount targets must cover the requested private subnets and NFS ingress must be limited to configured connector groups."
  }

  assert {
    condition     = length([for rule in aws_backup_plan.workspace.rule : rule if rule.lifecycle[0].delete_after == 35]) == 1
    error_message = "Workspace EFS must receive daily backups with the configured retention."
  }

  assert {
    condition     = aws_backup_selection.workspace.resources == toset([aws_efs_file_system.workspace.arn])
    error_message = "Workspace backup selection must include only the exact EFS filesystem."
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
    if statement.Sid == "DenyInsecureTransport" && statement.Condition.Bool["aws:SecureTransport"] == "false"]) == 1
    error_message = "The filesystem policy must deny all EFS actions without TLS."
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
      if statement.Sid == "AllowWorkspaceAccessPoint0OverTLS" && statement.Principal.AWS == "arn:aws:iam::123456789012:role/hako-ws-runtime-a" &&
      statement.Condition.Bool["aws:SecureTransport"] == "true" &&
    statement.Condition.ArnEquals["elasticfilesystem:AccessPointArn"] == "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"]) == 1
    error_message = "Each runtime role must be limited to its exact EFS Access Point ARN over TLS."
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
      if statement.Sid == "DenyWorkspaceRoleOutsideAccessPoint0" && statement.Effect == "Deny" &&
      statement.Principal.AWS == "arn:aws:iam::123456789012:role/hako-ws-runtime-a" &&
    statement.Condition.ArnNotEquals["elasticfilesystem:AccessPointArn"] == "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"]) == 1
    error_message = "An identity policy must not let a runtime role mount outside its bound Access Point."
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
      if statement.Sid == "DenyUnconfiguredWorkspaceRuntimeRole" && statement.Effect == "Deny" &&
      statement.Principal.AWS == "*" &&
    statement.Condition.ArnNotLike["aws:PrincipalArn"] == ["arn:aws:iam::123456789012:role/hako-ws-runtime-a"]]) == 1
    error_message = "Unconfigured principals must not mount even if an identity policy grants EFS access."
  }

  assert {
    condition     = output.file_system_id == aws_efs_file_system.workspace.id && output.backup_plan_id == aws_backup_plan.workspace.id
    error_message = "Storage and backup resource identifiers must be exposed for Resource Plane wiring."
  }
}

run "workspace_storage_dynamic_roles_require_scoped_identity_denies" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id                    = "rp-test"
    vpc_id                               = "vpc-0123456789abcdef0"
    private_subnet_ids                   = ["subnet-11111111", "subnet-22222222"]
    microvm_security_group_ids           = ["sg-11111111"]
    workspace_access_point_role_bindings = []
    enable_dynamic_workspace_roles       = true
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
      if statement.Sid == "AllowDynamicWorkspaceRolesThroughEFSAccessPoints" && statement.Effect == "Allow" &&
      statement.Principal.AWS == "arn:aws:iam::123456789012:root" &&
      statement.Condition.Bool["aws:SecureTransport"] == "true" &&
      statement.Condition.ArnLike["aws:PrincipalArn"] == "arn:aws:iam::123456789012:role/hako/workspaces/*" &&
    statement.Condition.ArnLike["elasticfilesystem:AccessPointArn"] == "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-*"]) == 1
    error_message = "Dynamic EFS resource policy must be limited to Hako Workspace role path, AP ARN, and TLS."
  }

  assert {
    condition = length([for statement in jsondecode(local.filesystem_policy).Statement : statement
      if statement.Sid == "DenyUnconfiguredWorkspaceRuntimeRole" &&
    statement.Condition.ArnNotLike["aws:PrincipalArn"] == ["arn:aws:iam::123456789012:role/hako/workspaces/*"]]) == 1
    error_message = "Dynamic mode must deny every principal outside the generated Workspace role path."
  }
}

run "workspace_storage_rejects_missing_network_and_shared_identity_inputs" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id                    = "rp-test"
    vpc_id                               = "vpc-0123456789abcdef0"
    private_subnet_ids                   = ["subnet-11111111"]
    microvm_security_group_ids           = []
    workspace_access_point_role_bindings = []
  }

  expect_failures = [
    var.private_subnet_ids,
    var.microvm_security_group_ids,
    var.workspace_access_point_role_bindings,
  ]
}

run "workspace_storage_rejects_reused_role_bindings" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id          = "rp-test"
    vpc_id                     = "vpc-0123456789abcdef0"
    private_subnet_ids         = ["subnet-11111111", "subnet-22222222"]
    microvm_security_group_ids = ["sg-11111111"]
    workspace_access_point_role_bindings = [
      {
        role_arn         = "arn:aws:iam::123456789012:role/hako-ws-runtime-a"
        access_point_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"
      },
      {
        role_arn         = "arn:aws:iam::123456789012:role/hako-ws-runtime-a"
        access_point_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-22222222"
      },
    ]
  }

  expect_failures = [
    var.workspace_access_point_role_bindings,
  ]
}

run "workspace_storage_rejects_reused_access_point_bindings" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id          = "rp-test"
    vpc_id                     = "vpc-0123456789abcdef0"
    private_subnet_ids         = ["subnet-11111111", "subnet-22222222"]
    microvm_security_group_ids = ["sg-11111111"]
    workspace_access_point_role_bindings = [
      {
        role_arn         = "arn:aws:iam::123456789012:role/hako-ws-runtime-a"
        access_point_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"
      },
      {
        role_arn         = "arn:aws:iam::123456789012:role/hako-ws-runtime-b"
        access_point_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:access-point/fsap-11111111"
      },
    ]
  }

  expect_failures = [
    var.workspace_access_point_role_bindings,
  ]
}
