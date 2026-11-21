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

variable "control_plane_preview_result_consumer_role_arn" {
  description = "Exact Control Plane Preview result consumer role ARN; required only when Preview runtime deployment is configured."
  type        = string
  default     = ""

  validation {
    condition     = var.control_plane_preview_result_consumer_role_arn == "" || can(regex("^arn:[^:]+:iam::[0-9]{12}:role/.+$", var.control_plane_preview_result_consumer_role_arn))
    error_message = "control_plane_preview_result_consumer_role_arn must be an IAM role ARN when set."
  }
}

variable "enable_workload_result_queue" {
  description = "Opt in to a separate Workload Run result queue/DLQ and alarms."
  type        = bool
  default     = false
}

variable "actions_runner_vpc_config" {
  description = "Pre-provisioned private subnets and dedicated security group for an explicitly configured Actions Runner worker."
  type = object({
    subnet_ids         = list(string)
    security_group_ids = list(string)
  })
  default = null
}

variable "workspace_service_controller_vpc_config" {
  description = "Pre-provisioned private subnets and dedicated security group for an explicitly configured Workspace Service Lambda consumer."
  type = object({
    subnet_ids         = list(string)
    security_group_ids = list(string)
  })
  default = null
}

variable "workspace_service_controller_deployment" {
  description = "Default-off Workspace Service Lambda MicroVM consumer. Supply a built ZIP, journal bucket/prefix, exact image/Secret ARNs and HTTPS report endpoint; never provide Secret values."
  type = object({
    zip_path                       = string
    journal_bucket_arn             = string
    journal_prefix                 = string
    microvm_image_arn              = string
    ticket_key_id                  = string
    ticket_key_secret_arn          = string
    report_key_secret_arn          = string
    report_endpoint                = string
    lambda_timeout_seconds         = number
    lambda_memory_size             = number
    reserved_concurrent_executions = number
    secret_kms_key_arns            = optional(list(string), [])
  })
  default = null
}

variable "actions_runner_deployment" {
  description = "Default-off Actions Runner Lambda MicroVM deployment. Supply exact Secret ARNs and a built ZIP, never Secret values."
  type = object({
    zip_path                       = string
    broker_url                     = string
    diagnostics_upload_base_url    = string
    broker_client_cert_secret_arn  = string
    broker_client_key_secret_arn   = string
    broker_ca_secret_arn           = string
    bootstrap_signing_key_arn      = string
    microvm_image_arn              = string
    microvm_image_version          = string
    execution_role_arn             = optional(string, "")
    egress_connector_arn           = string
    maximum_duration_seconds       = number
    startup_timeout_seconds        = number
    lambda_timeout_seconds         = number
    lambda_memory_size             = number
    reserved_concurrent_executions = number
    secret_kms_key_arns            = optional(list(string), [])
  })
  default = null
}

variable "actions_runner_cleanup_deployment" {
  description = "Optional default-off scheduled Actions Runner cleanup configuration; requires actions_runner_deployment and exact broker Secrets there. Never provide Secret values."
  type = object({
    schedule_expression            = optional(string, "rate(5 minutes)")
    lambda_timeout_seconds         = optional(number, 900)
    lambda_memory_size             = optional(number, 1024)
    reserved_concurrent_executions = optional(number, 1)
    batch_size                     = optional(number, 4)
    lease_duration_seconds         = optional(number, 120)
    operation_timeout_seconds      = optional(number, 60)
    poll_interval_milliseconds     = optional(number, 250)
    poll_timeout_seconds           = optional(number, 30)
    secret_kms_key_arns            = optional(list(string), [])
  })
  default = null
}

variable "control_plane_workload_result_consumer_role_arn" {
  description = "Exact Control Plane role allowed to consume Workload Run results; required when the queue is enabled."
  type        = string
  default     = ""
}

variable "workload_controller_secret_arns" {
  description = "Exact Workload Controller Secret ARNs for policy wiring; keep empty until a reviewed worker deployment. Never provide Secret values."
  type        = list(string)
  default     = []
}

variable "workload_controller_runtime" {
  description = "Optional exact Lambda MicroVM/S3 runtime IAM scope; null keeps runtime API and artifact permissions disabled. It does not create AWS resources."
  type = object({
    microvm_image_arn         = string
    ingress_connector_arn     = string
    egress_connector_arn      = string
    journal_bucket_arn        = string
    journal_prefix            = string
    recovery_lease_object_arn = string
    source_bucket_arn         = string
    source_prefix             = string
    source_kms_key_arn        = optional(string, "")
    artifact_bucket_arn       = string
    artifact_prefix           = string
    artifact_kms_key_arn      = optional(string, "")
    proposal_broker_role_arn  = optional(string, "")
    secret_kms_key_arns       = optional(list(string), [])
  })
  default = null
}

variable "workload_artifact_storage" {
  description = "Optional managed private bucket and lifecycle for the configured Workload artifact prefix. Requires workload_controller_runtime; null leaves the external bucket unmanaged."
  type = object({
    retention_days = number
  })
  default = null
}

variable "workload_controller_vpc_config" {
  description = "Private subnet and security group IDs for the optional Workload Controller Lambda."
  type = object({
    subnet_ids         = list(string)
    security_group_ids = list(string)
  })
  default = null
}

variable "workload_controller_deployment" {
  description = "Optional default-off combined Workload Controller Lambda deployment. Only file paths, endpoints, profile data, and exact Secret ARNs are accepted; never provide Secret values."
  type = object({
    zip_path                             = string
    control_plane_endpoint               = string
    control_plane_hmac_secret_arn        = string
    artifact_cleanup_hmac_secret_arn     = string
    broker_url                           = string
    broker_client_cert_secret_arn        = string
    broker_client_key_secret_arn         = string
    broker_ca_secret_arn                 = string
    ticket_key_id                        = string
    ticket_private_key_secret_arn        = string
    runtime_profiles_json                = string
    recovery_page_size                   = number
    recovery_max_pages                   = number
    recovery_lease_ttl                   = string
    recovery_schedule_expression         = string
    artifact_cleanup_batch_size          = number
    artifact_cleanup_schedule_expression = string
    lambda_timeout_seconds               = number
    lambda_memory_size                   = number
    reserved_concurrent_executions       = number
  })
  default = null
}

variable "workspace_storage" {
  description = "Optional encrypted EFS Workspace storage deployment; null keeps storage disabled. Static role/AP pairs or the reviewed dynamic Workspace role mode may be selected."
  type = object({
    vpc_id                     = string
    private_subnet_ids         = list(string)
    microvm_security_group_ids = list(string)
    workspace_access_point_role_bindings = optional(list(object({
      role_arn         = string
      access_point_arn = string
    })), [])
    enable_dynamic_workspace_roles = optional(bool, false)
    backup_schedule                = optional(string, "cron(0 5 * * ? *)")
    backup_retention_days          = optional(number, 35)
  })
  default = null
}

variable "workspace_gateway_deployment" {
  description = "Optional default-off HTTPS/WSS Fargate Gateway deployment contract. The object must contain Secret ARNs only, never Secret values."
  type        = any
  default     = null
}

variable "resource_plane_network_observability" {
  description = "Optional VPC Flow Logs for a caller-owned Resource Plane VPC. No networking is created or modified."
  type = object({
    vpc_id             = string
    log_retention_days = optional(number, 90)
    kms_key_arn        = optional(string, "")
  })
  default = null
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

variable "workspace_runtime_deployment" {
  description = "Default-off Lambda MicroVM Workspace controller deployment. Requires a reviewed ARM64 ZIP, exact journal/image/network profile, and dynamic EFS Workspace roles; leave null until Tasks 44/45/61 runtime gates are approved."
  type = object({
    zip_path                       = string
    journal_bucket_arn             = string
    journal_bucket_name            = string
    journal_prefix                 = string
    journal_kms_key_arn            = optional(string, "")
    runtime_class                  = string
    microvm_image_arn              = string
    microvm_image_version          = string
    egress_network_connectors      = list(string)
    ingress_network_connectors     = optional(list(string), [])
    maximum_duration_seconds       = number
    auto_resume_enabled            = bool
    max_idle_duration_seconds      = number
    suspended_duration_seconds     = number
    lambda_timeout_seconds         = number
    operation_timeout_seconds      = number
    lambda_memory_size             = number
    reserved_concurrent_executions = optional(number, 1)
  })
  default = null

  validation {
    condition = var.workspace_runtime_deployment == null || (
      var.workspace_storage != null && var.workspace_storage.enable_dynamic_workspace_roles
    )
    error_message = "Workspace Runtime Lambda deployment requires workspace_storage with enable_dynamic_workspace_roles=true."
  }
}

variable "enable_preview_runtime" {
  description = "Default-off Preview command queues and consumer on the Lambda MicroVM Workspace Resource Controller."
  type        = bool
  default     = false
}

variable "preview_runtime_source" {
  description = "Default-off Preview checkout broker, exact source archive prefix, Gateway origin, and Guest ticket secret configuration. Required when enable_preview_runtime is true."
  type = object({
    checkout_broker_url    = string
    broker_client_cert_arn = string
    broker_client_key_arn  = string
    broker_ca_arn          = string
    source_bucket_arn      = string
    source_bucket_name     = string
    source_prefix          = string
    source_kms_key_arn     = optional(string, "")
    gateway_origin         = string
    ticket_key_id          = string
    ticket_private_key_arn = string
  })
  default = null
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

variable "enable_health_report_alarms" {
  description = "Create health reporter alarms and its retained log group. Enable only after a reporter emits the documented metrics."
  type        = bool
  default     = false
}

variable "health_report_secret_arn" {
  description = "Optional exact Secret ARN for the Resource Plane HMAC key. Empty does not create a reporter role."
  type        = string
  default     = ""
}

variable "health_report_microvm_image_arn" {
  description = "Exact Lambda MicroVM image ARN checked by the optional health reporter."
  type        = string
  default     = ""
}

variable "health_report_microvm_image_version" {
  description = "Immutable numeric version of the Lambda MicroVM image checked by the optional health reporter."
  type        = string
  default     = ""
}

variable "health_report_vpc_egress_connector_arn" {
  description = "Exact Lambda Core VPC egress connector ARN checked by the optional health reporter."
  type        = string
  default     = ""
}

variable "health_report_controller_function_arn" {
  description = "Exact Workload controller Lambda function ARN checked by the optional health reporter."
  type        = string
  default     = ""
}

variable "health_report_workspace_efs_arn" {
  description = "Exact Workspace EFS file system ARN checked by the optional health reporter."
  type        = string
  default     = ""
}

variable "enable_health_reporter" {
  description = "Deploy the scheduled, opt-in health reporter Lambda. Requires the paired Plane Secret, Control Plane endpoint, built zip, and alarms."
  type        = bool
  default     = false
}

variable "health_reporter_lambda_zip_path" {
  description = "Built health reporter artifact path from this Terraform root."
  type        = string
  default     = "../../../../../build/hako-health-reporter.zip"
}

variable "health_report_control_plane_endpoint" {
  description = "HTTPS origin of the Control Plane API Gateway used for signed reports."
  type        = string
  default     = ""
}

variable "health_report_age_alarm_seconds" {
  description = "Resource Plane health report staleness threshold in seconds."
  type        = number
  default     = 300
}

variable "health_report_unhealthy_runs_threshold" {
  description = "Consecutive unhealthy probe runs before alerting."
  type        = number
  default     = 3
}
