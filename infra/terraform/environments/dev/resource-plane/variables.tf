variable "aws_region" {
  description = "AWS Region for this Resource Plane account."
  type        = string
  default     = "ap-northeast-1"
}

variable "resource_plane_id" {
  description = "Stable Hako Resource Plane identifier, not the AWS account ID."
  type        = string
  default     = "rp-dev"
}

variable "control_plane_dispatcher_role_arn" {
  description = "Exact Control Plane IAM role ARN allowed to send commands to this Resource Plane."
  type        = string
}

variable "control_plane_result_consumer_role_arn" {
  description = "Exact Control Plane IAM role ARN allowed to consume results from this Resource Plane."
  type        = string
}

variable "max_receive_count" {
  description = "Receives before messages move to the corresponding DLQ."
  type        = number
  default     = 5
}

variable "command_visibility_timeout_seconds" {
  description = "Command queue visibility timeout; coordinate with the future worker timeout."
  type        = number
  default     = 6000
}

variable "result_visibility_timeout_seconds" {
  description = "Result queue visibility timeout; coordinate with the future consumer timeout."
  type        = number
  default     = 6000
}

variable "message_retention_seconds" {
  description = "Retention for primary queues."
  type        = number
  default     = 345600
}

variable "dlq_retention_seconds" {
  description = "Retention for dead-letter queues."
  type        = number
  default     = 1209600
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention for a future Resource Controller Lambda."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Additional resource tags."
  type        = map(string)
  default     = {}
}
