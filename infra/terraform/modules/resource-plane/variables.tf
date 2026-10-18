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

variable "enable_workload_result_queue" {
  description = "Create separate Workload command/result queues and DLQs plus a queue-only worker role; the Workload result queue is not the Workspace Operation result queue."
  type        = bool
  default     = false
}

variable "control_plane_workload_result_consumer_role_arn" {
  description = "Exact Control Plane IAM role allowed to consume Workload Run results. Required only when enable_workload_result_queue is true."
  type        = string
  default     = ""

  validation {
    condition     = var.control_plane_workload_result_consumer_role_arn == "" || can(regex("^arn:[^:]+:iam::[0-9]{12}:role/.+$", var.control_plane_workload_result_consumer_role_arn))
    error_message = "control_plane_workload_result_consumer_role_arn must be empty or an IAM role ARN."
  }
  validation {
    condition     = !var.enable_workload_result_queue || var.control_plane_workload_result_consumer_role_arn != ""
    error_message = "control_plane_workload_result_consumer_role_arn is required when the Workload result queue is enabled."
  }
}

variable "workload_controller_secret_arns" {
  description = "Exact Resource Plane Secrets Manager ARNs readable by the Workload Controller role; values are never Terraform inputs. Leave empty until secret references and the worker deployment are reviewed."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for arn in var.workload_controller_secret_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]{1,512}$", arn))])
    error_message = "workload_controller_secret_arns must contain only concrete Secrets Manager secret ARNs."
  }
  validation {
    condition     = length(var.workload_controller_secret_arns) == length(toset(var.workload_controller_secret_arns))
    error_message = "workload_controller_secret_arns must not contain duplicates."
  }
  validation {
    condition     = var.enable_workload_result_queue || length(var.workload_controller_secret_arns) == 0
    error_message = "Workload Controller Secret permissions require the dedicated Workload command/result queues."
  }
  validation {
    condition     = length(var.workload_controller_secret_arns) == 0 || length(var.workload_controller_secret_arns) == 5
    error_message = "When configured, Workload Controller Secret permissions must include the five distinct runtime Secret ARNs."
  }
  validation {
    condition     = var.workload_controller_runtime == null || length(var.workload_controller_secret_arns) == 5
    error_message = "Runtime IAM requires all five exact Workload Controller Secret ARNs."
  }
  validation {
    condition     = length(var.workload_controller_secret_arns) == 0 || var.workload_controller_runtime != null
    error_message = "Workload Controller Secret access is enabled only together with the explicit runtime IAM scope."
  }
}

variable "workload_controller_runtime" {
  description = "Optional exact Lambda MicroVM and S3 runtime IAM scope. Null disables these permissions; this does not deploy or enable a Worker Lambda."
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
    secret_kms_key_arns       = optional(list(string), [])
  })
  default = null

  validation {
    condition = var.workload_controller_runtime == null || (
      var.enable_workload_result_queue &&
      can(regex("^arn:aws:lambda:[a-z0-9-]+:[0-9]{12}:microvm-image:[A-Za-z0-9_-]+$", var.workload_controller_runtime.microvm_image_arn)) &&
      var.workload_controller_runtime.ingress_connector_arn == "arn:aws:lambda:${split(":", var.workload_controller_runtime.microvm_image_arn)[3]}:aws:network-connector:aws-network-connector:ALL_INGRESS" &&
      can(regex("^arn:aws:lambda:[a-z0-9-]+:[0-9]{12}:network-connector:[A-Za-z0-9_-]+$", var.workload_controller_runtime.egress_connector_arn)) &&
      split(":", var.workload_controller_runtime.egress_connector_arn)[3] == split(":", var.workload_controller_runtime.microvm_image_arn)[3] &&
      split(":", var.workload_controller_runtime.egress_connector_arn)[4] == split(":", var.workload_controller_runtime.microvm_image_arn)[4] &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.workload_controller_runtime.journal_bucket_arn)) &&
      can(regex("^[A-Za-z0-9/_=-]{1,512}$", var.workload_controller_runtime.journal_prefix)) &&
      !startswith(var.workload_controller_runtime.journal_prefix, "/") && !endswith(var.workload_controller_runtime.journal_prefix, "/") && !can(regex("\\.\\.|//", var.workload_controller_runtime.journal_prefix)) &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/[A-Za-z0-9/_.=-]{1,512}$", var.workload_controller_runtime.recovery_lease_object_arn)) &&
      !can(regex("\\.\\.", var.workload_controller_runtime.recovery_lease_object_arn)) &&
      startswith(var.workload_controller_runtime.recovery_lease_object_arn, "${var.workload_controller_runtime.journal_bucket_arn}/${var.workload_controller_runtime.journal_prefix}/") &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.workload_controller_runtime.source_bucket_arn)) &&
      can(regex("^[A-Za-z0-9/_=-]{1,512}$", var.workload_controller_runtime.source_prefix)) &&
      !startswith(var.workload_controller_runtime.source_prefix, "/") && !endswith(var.workload_controller_runtime.source_prefix, "/") && !can(regex("\\.\\.|//", var.workload_controller_runtime.source_prefix)) &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.workload_controller_runtime.artifact_bucket_arn)) &&
      can(regex("^[A-Za-z0-9/_=-]{1,512}$", var.workload_controller_runtime.artifact_prefix)) &&
      !startswith(var.workload_controller_runtime.artifact_prefix, "/") && !endswith(var.workload_controller_runtime.artifact_prefix, "/") && !can(regex("\\.\\.|//", var.workload_controller_runtime.artifact_prefix)) &&
      (var.workload_controller_runtime.source_kms_key_arn == "" || can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", var.workload_controller_runtime.source_kms_key_arn))) &&
      (var.workload_controller_runtime.artifact_kms_key_arn == "" || can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", var.workload_controller_runtime.artifact_kms_key_arn))) &&
      alltrue([for arn in var.workload_controller_runtime.secret_kms_key_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", arn))]) &&
      length(var.workload_controller_runtime.secret_kms_key_arns) == length(toset(var.workload_controller_runtime.secret_kms_key_arns))
    )
    error_message = "Workload runtime IAM requires the dedicated queues, exact image/connectors, valid S3 bucket prefixes, an in-prefix lease object, and concrete optional KMS key ARNs."
  }
}

variable "workload_controller_vpc_config" {
  description = "Pre-provisioned private subnets and a dedicated security group for the Workload Controller Lambda. Network routes/endpoints remain owned and reviewed by the Resource Plane account."
  type = object({
    subnet_ids         = list(string)
    security_group_ids = list(string)
  })
  default = null

  validation {
    condition = var.workload_controller_vpc_config == null || (
      length(var.workload_controller_vpc_config.subnet_ids) >= 2 && length(var.workload_controller_vpc_config.subnet_ids) <= 16 &&
      length(var.workload_controller_vpc_config.subnet_ids) == length(toset(var.workload_controller_vpc_config.subnet_ids)) &&
      alltrue([for id in var.workload_controller_vpc_config.subnet_ids : can(regex("^subnet-[0-9A-Fa-f]+$", id))]) &&
      length(var.workload_controller_vpc_config.security_group_ids) >= 1 && length(var.workload_controller_vpc_config.security_group_ids) <= 5 &&
      length(var.workload_controller_vpc_config.security_group_ids) == length(toset(var.workload_controller_vpc_config.security_group_ids)) &&
      alltrue([for id in var.workload_controller_vpc_config.security_group_ids : can(regex("^sg-[0-9A-Fa-f]+$", id))])
    )
    error_message = "workload_controller_vpc_config requires 2-16 unique subnet IDs and 1-5 unique security group IDs."
  }
}

variable "workload_controller_deployment" {
  description = "Optional default-off combined Workload command/recovery Lambda deployment. Requires the dedicated Workload queues, exact runtime policy, and five Secret ARNs; Terraform never receives Secret values."
  type = object({
    zip_path                       = string
    control_plane_endpoint         = string
    control_plane_hmac_secret_arn  = string
    broker_url                     = string
    broker_client_cert_secret_arn  = string
    broker_client_key_secret_arn   = string
    broker_ca_secret_arn           = string
    ticket_key_id                  = string
    ticket_private_key_secret_arn  = string
    runtime_profiles_json          = string
    recovery_page_size             = number
    recovery_max_pages             = number
    recovery_lease_ttl             = string
    recovery_schedule_expression   = string
    lambda_timeout_seconds         = number
    lambda_memory_size             = number
    reserved_concurrent_executions = number
  })
  default = null

  validation {
    condition = var.workload_controller_deployment == null || (
      var.enable_workload_result_queue && var.workload_controller_runtime != null && var.workload_controller_vpc_config != null &&
      var.workload_controller_deployment.zip_path != "" && fileexists(var.workload_controller_deployment.zip_path) &&
      can(regex("^https://[^/]+", var.workload_controller_deployment.control_plane_endpoint)) &&
      can(regex("^https://[^/]+", var.workload_controller_deployment.broker_url)) &&
      var.workload_controller_deployment.ticket_key_id != "" &&
      length(var.workload_controller_deployment.runtime_profiles_json) > 0 && length(var.workload_controller_deployment.runtime_profiles_json) <= 32768 &&
      can(jsondecode(var.workload_controller_deployment.runtime_profiles_json)) &&
      var.workload_controller_deployment.recovery_page_size >= 1 && var.workload_controller_deployment.recovery_page_size <= 1000 &&
      floor(var.workload_controller_deployment.recovery_page_size) == var.workload_controller_deployment.recovery_page_size &&
      var.workload_controller_deployment.recovery_max_pages >= 1 && var.workload_controller_deployment.recovery_max_pages <= 1000 &&
      floor(var.workload_controller_deployment.recovery_max_pages) == var.workload_controller_deployment.recovery_max_pages &&
      can(regex("^[1-9][0-9]*[smh]$", var.workload_controller_deployment.recovery_lease_ttl)) &&
      can(regex("^(rate\\(1 (minute|hour|day)s?\\)|rate\\(([2-9]|[1-9][0-9]+) (minutes|hours|days)\\)|cron\\(.+\\))$", var.workload_controller_deployment.recovery_schedule_expression)) &&
      var.workload_controller_deployment.lambda_timeout_seconds >= 1 && var.workload_controller_deployment.lambda_timeout_seconds <= 900 &&
      floor(var.workload_controller_deployment.lambda_timeout_seconds) == var.workload_controller_deployment.lambda_timeout_seconds &&
      var.workload_controller_deployment.lambda_memory_size >= 1024 && var.workload_controller_deployment.lambda_memory_size <= 10240 &&
      floor(var.workload_controller_deployment.lambda_memory_size) == var.workload_controller_deployment.lambda_memory_size &&
      var.workload_controller_deployment.reserved_concurrent_executions >= 2 && var.workload_controller_deployment.reserved_concurrent_executions <= 1000 &&
      floor(var.workload_controller_deployment.reserved_concurrent_executions) == var.workload_controller_deployment.reserved_concurrent_executions &&
      var.command_visibility_timeout_seconds >= var.workload_controller_deployment.lambda_timeout_seconds * 6 &&
      toset([
        var.workload_controller_deployment.control_plane_hmac_secret_arn,
        var.workload_controller_deployment.broker_client_cert_secret_arn,
        var.workload_controller_deployment.broker_client_key_secret_arn,
        var.workload_controller_deployment.broker_ca_secret_arn,
        var.workload_controller_deployment.ticket_private_key_secret_arn
      ]) == toset(var.workload_controller_secret_arns)
    )
    error_message = "Workload Controller deployment requires an existing ZIP, HTTPS endpoints, private VPC subnets/security groups, exact five matching Secret ARNs, enabled dedicated queues/runtime IAM, bounded Lambda/recovery settings, at least two Lambda concurrency slots, and queue visibility at least six times its timeout."
  }
}

variable "actions_runner_vpc_config" {
  description = "Pre-provisioned private subnets and a dedicated security group for the optional Actions Runner Lambda."
  type = object({
    subnet_ids         = list(string)
    security_group_ids = list(string)
  })
  default = null

  validation {
    condition = var.actions_runner_vpc_config == null || (
      length(var.actions_runner_vpc_config.subnet_ids) >= 2 && length(var.actions_runner_vpc_config.subnet_ids) <= 16 &&
      length(var.actions_runner_vpc_config.subnet_ids) == length(toset(var.actions_runner_vpc_config.subnet_ids)) &&
      alltrue([for id in var.actions_runner_vpc_config.subnet_ids : can(regex("^subnet-[0-9A-Fa-f]+$", id))]) &&
      length(var.actions_runner_vpc_config.security_group_ids) >= 1 && length(var.actions_runner_vpc_config.security_group_ids) <= 5 &&
      length(var.actions_runner_vpc_config.security_group_ids) == length(toset(var.actions_runner_vpc_config.security_group_ids)) &&
      alltrue([for id in var.actions_runner_vpc_config.security_group_ids : can(regex("^sg-[0-9A-Fa-f]+$", id))])
    )
    error_message = "actions_runner_vpc_config requires unique private subnet and dedicated security group IDs."
  }
}

variable "actions_runner_deployment" {
  description = "Optional default-off Lambda MicroVM startup worker deployment; only ZIP path, policy inputs, endpoints, and exact Secret ARNs are accepted. Never provide Secret values."
  type = object({
    zip_path                       = string
    broker_url                     = string
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

  validation {
    condition = var.actions_runner_deployment == null || (
      var.actions_runner_vpc_config != null &&
      var.actions_runner_deployment.zip_path != "" && fileexists(var.actions_runner_deployment.zip_path) &&
      can(regex("^https://[^/?#]+(?::[0-9]+)?(/[^?#]*)?$", var.actions_runner_deployment.broker_url)) &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):lambda:[a-z0-9-]+:[0-9]{12}:microvm-image:[A-Za-z0-9_-]+$", var.actions_runner_deployment.microvm_image_arn)) &&
      can(regex("^[1-9][0-9]*$", var.actions_runner_deployment.microvm_image_version)) &&
      can(regex("^arn:(aws|aws-us-gov|aws-cn):lambda:[a-z0-9-]+:[0-9]{12}:network-connector:[A-Za-z0-9_-]+$", var.actions_runner_deployment.egress_connector_arn)) &&
      split(":", var.actions_runner_deployment.microvm_image_arn)[3] == split(":", var.actions_runner_deployment.egress_connector_arn)[3] &&
      split(":", var.actions_runner_deployment.microvm_image_arn)[4] == split(":", var.actions_runner_deployment.egress_connector_arn)[4] &&
      (var.actions_runner_deployment.execution_role_arn == "" || can(regex("^arn:(aws|aws-us-gov|aws-cn):iam::[0-9]{12}:role/.+$", var.actions_runner_deployment.execution_role_arn))) &&
      var.actions_runner_deployment.maximum_duration_seconds >= 1 && var.actions_runner_deployment.maximum_duration_seconds <= 28800 &&
      floor(var.actions_runner_deployment.maximum_duration_seconds) == var.actions_runner_deployment.maximum_duration_seconds &&
      var.actions_runner_deployment.startup_timeout_seconds >= 5 && var.actions_runner_deployment.startup_timeout_seconds <= 300 &&
      floor(var.actions_runner_deployment.startup_timeout_seconds) == var.actions_runner_deployment.startup_timeout_seconds &&
      var.actions_runner_deployment.lambda_timeout_seconds >= var.actions_runner_deployment.startup_timeout_seconds + 30 && var.actions_runner_deployment.lambda_timeout_seconds <= 900 &&
      floor(var.actions_runner_deployment.lambda_timeout_seconds) == var.actions_runner_deployment.lambda_timeout_seconds &&
      var.command_visibility_timeout_seconds >= var.actions_runner_deployment.lambda_timeout_seconds * 6 &&
      var.actions_runner_deployment.lambda_memory_size >= 1024 && var.actions_runner_deployment.lambda_memory_size <= 10240 &&
      floor(var.actions_runner_deployment.lambda_memory_size) == var.actions_runner_deployment.lambda_memory_size &&
      var.actions_runner_deployment.reserved_concurrent_executions >= 1 && var.actions_runner_deployment.reserved_concurrent_executions <= 1000 &&
      floor(var.actions_runner_deployment.reserved_concurrent_executions) == var.actions_runner_deployment.reserved_concurrent_executions &&
      length(toset([
        var.actions_runner_deployment.broker_client_cert_secret_arn,
        var.actions_runner_deployment.broker_client_key_secret_arn,
        var.actions_runner_deployment.broker_ca_secret_arn,
        var.actions_runner_deployment.bootstrap_signing_key_arn
      ])) == 4 &&
      alltrue([for arn in [var.actions_runner_deployment.broker_client_cert_secret_arn, var.actions_runner_deployment.broker_client_key_secret_arn,
        var.actions_runner_deployment.broker_ca_secret_arn, var.actions_runner_deployment.bootstrap_signing_key_arn] :
      can(regex("^arn:(aws|aws-us-gov|aws-cn):secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]{1,512}$", arn))]) &&
      alltrue([for arn in var.actions_runner_deployment.secret_kms_key_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", arn))]) &&
      length(var.actions_runner_deployment.secret_kms_key_arns) == length(toset(var.actions_runner_deployment.secret_kms_key_arns))
    )
    error_message = "Actions Runner deployment requires an existing ZIP, HTTPS broker, private VPC configuration, fixed MicroVM image/version and same-Region egress connector, four distinct exact Secret ARNs, bounded timeouts/concurrency, and queue visibility at least six times the Lambda timeout."
  }
}

variable "actions_runner_cleanup_deployment" {
  description = "Optional default-off scheduled cleanup for Actions Runner MicroVMs. Requires the startup deployment's reviewed ZIP, broker URL, exact mTLS Secrets, image ARN, and private VPC configuration."
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

  validation {
    condition = var.actions_runner_cleanup_deployment == null || (
      var.actions_runner_deployment != null && var.actions_runner_vpc_config != null &&
      can(regex("^rate\\([1-9][0-9]* (minute|minutes|hour|hours)\\)$", var.actions_runner_cleanup_deployment.schedule_expression)) &&
      var.actions_runner_cleanup_deployment.lambda_timeout_seconds >= 60 && var.actions_runner_cleanup_deployment.lambda_timeout_seconds <= 900 &&
      floor(var.actions_runner_cleanup_deployment.lambda_timeout_seconds) == var.actions_runner_cleanup_deployment.lambda_timeout_seconds &&
      var.actions_runner_cleanup_deployment.lambda_memory_size >= 1024 && var.actions_runner_cleanup_deployment.lambda_memory_size <= 10240 &&
      floor(var.actions_runner_cleanup_deployment.lambda_memory_size) == var.actions_runner_cleanup_deployment.lambda_memory_size &&
      var.actions_runner_cleanup_deployment.reserved_concurrent_executions == 1 &&
      var.actions_runner_cleanup_deployment.batch_size >= 1 && var.actions_runner_cleanup_deployment.batch_size <= 100 &&
      floor(var.actions_runner_cleanup_deployment.batch_size) == var.actions_runner_cleanup_deployment.batch_size &&
      var.actions_runner_cleanup_deployment.lease_duration_seconds >= 5 && var.actions_runner_cleanup_deployment.lease_duration_seconds <= 600 &&
      floor(var.actions_runner_cleanup_deployment.lease_duration_seconds) == var.actions_runner_cleanup_deployment.lease_duration_seconds &&
      var.actions_runner_cleanup_deployment.operation_timeout_seconds >= 1 && var.actions_runner_cleanup_deployment.operation_timeout_seconds <= 300 &&
      floor(var.actions_runner_cleanup_deployment.operation_timeout_seconds) == var.actions_runner_cleanup_deployment.operation_timeout_seconds &&
      var.actions_runner_cleanup_deployment.operation_timeout_seconds + 5 <= var.actions_runner_cleanup_deployment.lease_duration_seconds &&
      var.actions_runner_cleanup_deployment.poll_interval_milliseconds >= 10 && var.actions_runner_cleanup_deployment.poll_interval_milliseconds <= 60000 &&
      floor(var.actions_runner_cleanup_deployment.poll_interval_milliseconds) == var.actions_runner_cleanup_deployment.poll_interval_milliseconds &&
      var.actions_runner_cleanup_deployment.poll_timeout_seconds >= ceil(var.actions_runner_cleanup_deployment.poll_interval_milliseconds / 1000) &&
      var.actions_runner_cleanup_deployment.poll_timeout_seconds <= var.actions_runner_cleanup_deployment.operation_timeout_seconds &&
      floor(var.actions_runner_cleanup_deployment.poll_timeout_seconds) == var.actions_runner_cleanup_deployment.poll_timeout_seconds &&
      var.actions_runner_cleanup_deployment.batch_size * (var.actions_runner_cleanup_deployment.operation_timeout_seconds + 15) <= var.actions_runner_cleanup_deployment.lambda_timeout_seconds &&
      alltrue([for arn in var.actions_runner_cleanup_deployment.secret_kms_key_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", arn))]) &&
      length(var.actions_runner_cleanup_deployment.secret_kms_key_arns) == length(toset(var.actions_runner_cleanup_deployment.secret_kms_key_arns))
    )
    error_message = "Actions Runner cleanup requires its opt-in startup deployment/private VPC, a rate schedule, concurrency 1, bounded lease/operation/poll/batch settings, enough Lambda timeout for a full batch, and valid optional KMS key ARNs."
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

variable "enable_health_report_alarms" {
  description = "Create Resource Plane health freshness, probe failure, repeated unhealthy, and reporter outage alarms. Enable only with a reporter that emits the documented custom metrics."
  type        = bool
  default     = false
}

variable "enable_health_reporter" {
  description = "Deploy a scheduled Lambda that checks this Plane's command/result queues, exact Lambda MicroVM image version, VPC egress connector, controller function, and Workspace EFS, sends a signed report, and emits EMF metrics. Requires exact resource ARNs, key Secret, API endpoint, Lambda zip, and alarms."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_health_reporter || (var.health_report_secret_arn != "" && var.health_report_control_plane_endpoint != "" && var.health_report_microvm_image_arn != "" && var.health_report_microvm_image_version != "" && var.health_report_vpc_egress_connector_arn != "" && var.health_report_controller_function_arn != "" && var.health_report_workspace_efs_arn != "" && var.enable_health_report_alarms)
    error_message = "The health reporter requires a key Secret ARN, Control Plane HTTPS endpoint, fixed MicroVM image/version, VPC connector, controller function ARN, Workspace EFS ARN, and enabled health alarms."
  }
}

variable "health_reporter_lambda_zip_path" {
  description = "Path to the ARM64 custom-runtime zip produced by make build-health-reporter-lambda."
  type        = string
  default     = "../../../../../build/hako-health-reporter.zip"

  validation {
    condition     = !var.enable_health_reporter || fileexists(var.health_reporter_lambda_zip_path)
    error_message = "Build the health reporter Lambda zip before enabling the reporter."
  }
}

variable "health_report_control_plane_endpoint" {
  description = "HTTPS API Gateway origin for the Hako signed Resource Plane health endpoint."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_control_plane_endpoint == "" || can(regex("^https://[^/]+/?$", var.health_report_control_plane_endpoint))
    error_message = "health_report_control_plane_endpoint must be empty or an HTTPS origin without a path."
  }
}

variable "health_report_secret_arn" {
  description = "Exact Secret ARN containing this Resource Plane's HMAC key. Used only when the scheduled reporter is enabled; Terraform does not manage or retrieve Secret values."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_secret_arn == "" || can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.health_report_secret_arn))
    error_message = "health_report_secret_arn must be empty or an exact Secrets Manager Secret ARN."
  }
}

variable "health_report_microvm_image_arn" {
  description = "Exact Lambda MicroVM image ARN probed by the health reporter."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_microvm_image_arn == "" || can(regex("^arn:[^:]+:lambda:[^:]+:[0-9]{12}:microvm-image:[^:]+$", var.health_report_microvm_image_arn))
    error_message = "health_report_microvm_image_arn must be empty or an exact Lambda MicroVM image ARN."
  }
}

variable "health_report_microvm_image_version" {
  description = "Immutable version of the Lambda MicroVM image probed by the health reporter."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_microvm_image_version == "" || can(regex("^[1-9][0-9]*$", var.health_report_microvm_image_version))
    error_message = "health_report_microvm_image_version must be empty or a positive numeric version."
  }
}

variable "health_report_vpc_egress_connector_arn" {
  description = "Exact Lambda Core VPC egress connector ARN checked by the health reporter."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_vpc_egress_connector_arn == "" || can(regex("^arn:[^:]+:lambda:[^:]+:[0-9]{12}:network-connector:[^:]+$", var.health_report_vpc_egress_connector_arn))
    error_message = "health_report_vpc_egress_connector_arn must be empty or an exact network connector ARN."
  }
}

variable "health_report_controller_function_arn" {
  description = "Exact Resource Plane Workload controller Lambda function ARN checked by the health reporter."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_controller_function_arn == "" || can(regex("^arn:aws:lambda:[a-z0-9-]+:[0-9]{12}:function:[A-Za-z0-9_-]{1,64}$", var.health_report_controller_function_arn))
    error_message = "health_report_controller_function_arn must be empty or an exact unqualified Lambda function ARN."
  }
}

variable "health_report_workspace_efs_arn" {
  description = "Exact Workspace EFS file system ARN checked by the health reporter."
  type        = string
  default     = ""

  validation {
    condition     = var.health_report_workspace_efs_arn == "" || can(regex("^arn:aws:elasticfilesystem:[a-z0-9-]+:[0-9]{12}:file-system/fs-[0-9a-f]{8,40}$", var.health_report_workspace_efs_arn))
    error_message = "health_report_workspace_efs_arn must be empty or an exact EFS file system ARN."
  }
}

variable "health_report_age_alarm_seconds" {
  description = "Stale report alarm threshold in seconds; align with the Control Plane health freshness TTL."
  type        = number
  default     = 300

  validation {
    condition     = var.health_report_age_alarm_seconds >= 60 && var.health_report_age_alarm_seconds <= 86400
    error_message = "health_report_age_alarm_seconds must be between 60 and 86400."
  }
}

variable "health_report_unhealthy_runs_threshold" {
  description = "Alarm when consecutive unhealthy probe runs reach this count."
  type        = number
  default     = 3

  validation {
    condition     = var.health_report_unhealthy_runs_threshold >= 1 && var.health_report_unhealthy_runs_threshold <= 1000
    error_message = "health_report_unhealthy_runs_threshold must be between 1 and 1000."
  }
}
