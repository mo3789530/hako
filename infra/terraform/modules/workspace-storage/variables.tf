variable "resource_plane_id" {
  description = "Stable Hako Resource Plane identifier used in names and tags."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.resource_plane_id))
    error_message = "resource_plane_id must be a lowercase stable identifier of 2–31 characters."
  }
}

variable "vpc_id" {
  description = "VPC containing the private Lambda MicroVM connector and EFS mount targets."
  type        = string

  validation {
    condition     = can(regex("^vpc-[A-Za-z0-9]+$", var.vpc_id))
    error_message = "vpc_id must be a VPC ID."
  }
}

variable "private_subnet_ids" {
  description = "Private subnet IDs for EFS mount targets; each must be in a distinct AZ and the configured VPC."
  type        = list(string)

  validation {
    condition = (
      length(var.private_subnet_ids) >= 2 &&
      length(var.private_subnet_ids) == length(distinct(var.private_subnet_ids)) &&
      alltrue([for id in var.private_subnet_ids : can(regex("^subnet-[A-Za-z0-9]+$", id))])
    )
    error_message = "private_subnet_ids must contain at least two unique subnet IDs."
  }
}

variable "microvm_security_group_ids" {
  description = "Exact security groups attached to the approved Lambda MicroVM VPC connector; only these receive EFS TCP/2049 ingress."
  type        = list(string)

  validation {
    condition = (
      length(var.microvm_security_group_ids) > 0 &&
      length(var.microvm_security_group_ids) == length(distinct(var.microvm_security_group_ids)) &&
      alltrue([for id in var.microvm_security_group_ids : can(regex("^sg-[A-Za-z0-9]+$", id))])
    )
    error_message = "microvm_security_group_ids must contain unique security group IDs."
  }
}

variable "workspace_runtime_role_arns" {
  description = "Exact IAM role ARNs allowed to mount/write through EFS Access Points. Do not use a shared developer or Control Plane role."
  type        = list(string)

  validation {
    condition = (
      length(var.workspace_runtime_role_arns) > 0 &&
      length(var.workspace_runtime_role_arns) == length(distinct(var.workspace_runtime_role_arns)) &&
      alltrue([for arn in var.workspace_runtime_role_arns : can(regex("^arn:[^:]+:iam::[0-9]{12}:role/.+$", arn))])
    )
    error_message = "workspace_runtime_role_arns must contain unique exact IAM role ARNs."
  }
}

variable "backup_schedule" {
  description = "AWS Backup cron expression for Workspace EFS snapshots (UTC)."
  type        = string
  default     = "cron(0 5 * * ? *)"

  validation {
    condition     = can(regex("^cron\\([0-9*/?, -]+ [0-9*/?, -]+ [0-9*/?, -]+ [0-9*/?, -]+ [0-9*/?, -]+ [0-9*/?, -]+\\)$", var.backup_schedule))
    error_message = "backup_schedule must be a six-field AWS Backup cron expression."
  }
}

variable "backup_retention_days" {
  description = "Daily EFS recovery point retention window."
  type        = number
  default     = 35

  validation {
    condition     = var.backup_retention_days >= 7 && var.backup_retention_days <= 3650
    error_message = "backup_retention_days must be between 7 and 3650."
  }
}

variable "tags" {
  description = "Additional tags applied to managed storage and backup resources."
  type        = map(string)
  default     = {}
}
