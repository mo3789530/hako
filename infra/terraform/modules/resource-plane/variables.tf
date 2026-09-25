variable "resource_plane_id" {
  description = "Stable Hako Resource Plane identifier; not an AWS account ID."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,31}$", var.resource_plane_id))
    error_message = "resource_plane_id must be 1-32 lowercase letters, numbers, or hyphens, beginning with a letter or number."
  }
}

variable "control_plane_dispatcher_role_arn" {
  description = "Exact IAM role ARN allowed to send Resource Plane commands."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:iam::[0-9]{12}:role/.+$", var.control_plane_dispatcher_role_arn))
    error_message = "control_plane_dispatcher_role_arn must be an IAM role ARN."
  }
}

variable "control_plane_result_consumer_role_arn" {
  description = "Exact IAM role ARN allowed to receive Resource Plane results."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:iam::[0-9]{12}:role/.+$", var.control_plane_result_consumer_role_arn))
    error_message = "control_plane_result_consumer_role_arn must be an IAM role ARN."
  }
}

variable "max_receive_count" {
  description = "Receives before SQS moves a command/result message to its DLQ."
  type        = number
  default     = 5

  validation {
    condition     = var.max_receive_count >= 1 && var.max_receive_count <= 1000
    error_message = "max_receive_count must be between 1 and 1000."
  }
}

variable "command_visibility_timeout_seconds" {
  description = "Visibility timeout for command workers; align with the eventual Lambda timeout and batch window."
  type        = number
  default     = 6000

  validation {
    condition     = var.command_visibility_timeout_seconds >= 1 && var.command_visibility_timeout_seconds <= 43200
    error_message = "command_visibility_timeout_seconds must be between 1 and 43200."
  }
}

variable "result_visibility_timeout_seconds" {
  description = "Visibility timeout for result consumers; align with the eventual consumer timeout and batch window."
  type        = number
  default     = 6000

  validation {
    condition     = var.result_visibility_timeout_seconds >= 1 && var.result_visibility_timeout_seconds <= 43200
    error_message = "result_visibility_timeout_seconds must be between 1 and 43200."
  }
}

variable "message_retention_seconds" {
  description = "Retention for command and result queues."
  type        = number
  default     = 345600

  validation {
    condition     = var.message_retention_seconds >= 60 && var.message_retention_seconds <= 1209600
    error_message = "message_retention_seconds must be between 60 and 1209600."
  }
}

variable "dlq_retention_seconds" {
  description = "Retention for command and result dead-letter queues."
  type        = number
  default     = 1209600

  validation {
    condition     = var.dlq_retention_seconds >= 60 && var.dlq_retention_seconds <= 1209600
    error_message = "dlq_retention_seconds must be between 60 and 1209600."
  }
}

variable "log_retention_days" {
  description = "Resource Controller CloudWatch Logs retention."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Additional tags applied to Resource Plane infrastructure."
  type        = map(string)
  default     = {}
}
