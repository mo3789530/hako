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

variable "enable_fake_resource_controller" {
  description = "Opt in to a development-only Lambda that uses the in-memory Fake Runtime; never a production Workspace executor."
  type        = bool
  default     = false
}

variable "fake_controller_lambda_zip_path" {
  description = "Path from the Terraform root to the zip produced by make build-resource-controller-lambda."
  type        = string
  default     = "../../../../../build/resource-controller-lambda.zip"

  validation {
    condition     = !var.enable_fake_resource_controller || fileexists(var.fake_controller_lambda_zip_path)
    error_message = "Build the fake Resource Controller Lambda zip before enabling it."
  }
}

variable "fake_controller_timeout_seconds" {
  description = "Development-only fake controller Lambda timeout."
  type        = number
  default     = 60

  validation {
    condition     = var.fake_controller_timeout_seconds >= 1 && var.fake_controller_timeout_seconds <= 900
    error_message = "fake_controller_timeout_seconds must be between 1 and 900."
  }
}

variable "fake_controller_execution_timeout_seconds" {
  description = "Maximum time the fake Runtime operation handler may run."
  type        = number
  default     = 45

  validation {
    condition     = var.fake_controller_execution_timeout_seconds >= 1 && var.fake_controller_execution_timeout_seconds <= 899
    error_message = "fake_controller_execution_timeout_seconds must be between 1 and 899."
  }
}

variable "fake_controller_memory_size" {
  description = "Development-only fake controller Lambda memory."
  type        = number
  default     = 512

  validation {
    condition     = var.fake_controller_memory_size >= 128 && var.fake_controller_memory_size <= 10240
    error_message = "fake_controller_memory_size must be between 128 and 10240 MB."
  }
}

variable "tags" {
  description = "Additional tags applied to Resource Plane infrastructure."
  type        = map(string)
  default     = {}
}

variable "alarm_actions" {
  description = "Optional SNS topic ARNs notified when Resource Plane monitoring alarms enter ALARM state."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for arn in var.alarm_actions : can(regex("^arn:[^:]+:sns:[^:]+:[0-9]{12}:.+$", arn))])
    error_message = "alarm_actions entries must be SNS topic ARNs."
  }
}

variable "oldest_message_age_alarm_seconds" {
  description = "Alarm when the oldest primary queue message exceeds this age."
  type        = number
  default     = 300

  validation {
    condition     = var.oldest_message_age_alarm_seconds > 0
    error_message = "oldest_message_age_alarm_seconds must be greater than zero."
  }
}

variable "queue_backlog_alarm_messages" {
  description = "Alarm when visible messages on a primary queue exceed this count."
  type        = number
  default     = 100

  validation {
    condition     = var.queue_backlog_alarm_messages > 0
    error_message = "queue_backlog_alarm_messages must be greater than zero."
  }
}
