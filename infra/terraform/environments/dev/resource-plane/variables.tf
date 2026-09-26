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

variable "enable_fake_resource_controller" {
  description = "Explicitly enable the dev-only Lambda backed by the process-local Fake Runtime."
  type        = bool
  default     = false
}

variable "fake_controller_lambda_zip_path" {
  description = "Built artifact path from this Terraform root; make build-resource-controller-lambda creates the default."
  type        = string
  default     = "../../../../../build/resource-controller-lambda.zip"
}

variable "fake_controller_timeout_seconds" {
  description = "Development fake controller Lambda timeout."
  type        = number
  default     = 60
}

variable "fake_controller_execution_timeout_seconds" {
  description = "Development fake Runtime execution timeout."
  type        = number
  default     = 45
}

variable "fake_controller_memory_size" {
  description = "Development fake controller Lambda memory."
  type        = number
  default     = 512
}

variable "tags" {
  description = "Additional resource tags."
  type        = map(string)
  default     = {}
}

variable "alarm_actions" {
  description = "Optional SNS topic ARNs to notify for Resource Plane CloudWatch alarms."
  type        = list(string)
  default     = []
}

variable "oldest_message_age_alarm_seconds" {
  description = "Primary queue oldest-message age alarm threshold in seconds."
  type        = number
  default     = 300
}

variable "queue_backlog_alarm_messages" {
  description = "Primary queue visible-message count alarm threshold."
  type        = number
  default     = 100
}
