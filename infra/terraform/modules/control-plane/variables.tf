variable "api_name" {
  description = "Name of the Hako Control Plane HTTP API."
  type        = string
  default     = "hako-control-plane"
}

variable "aws_region" {
  description = "Region used to construct the Aurora DSQL IAM-authenticated endpoint."
  type        = string
}

variable "api_lambda_zip_path" {
  description = "Path to the arm64 custom-runtime ZIP produced by make build-api-lambda."
  type        = string
}

variable "github_webhook_secret_arn" {
  description = "Optional Secrets Manager ARN holding the GitHub App webhook SecretString. Empty disables the unauthenticated webhook route."
  type        = string
  default     = ""

  validation {
    condition     = var.github_webhook_secret_arn == "" || can(regex("^arn:[a-z0-9-]+:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:.+$", var.github_webhook_secret_arn))
    error_message = "github_webhook_secret_arn must be empty or a Secrets Manager secret ARN."
  }
}

variable "tenant_github_webhook_enabled" {
  description = "Enable the opaque Tenant-owned GitHub App Webhook ingress. Requires explicitly enumerated Webhook-purpose reader Roles; disabled by default."
  type        = bool
  default     = false
}

variable "tenant_github_webhook_throttle_rate" {
  description = "API Gateway per-route request rate for public Tenant GitHub webhook ingress."
  type        = number
  default     = 50

  validation {
    condition     = var.tenant_github_webhook_throttle_rate > 0 && var.tenant_github_webhook_throttle_rate <= 10000
    error_message = "Tenant GitHub Webhook throttle rate must be between 0 and 10000 requests per second."
  }
}

variable "tenant_github_webhook_throttle_burst" {
  description = "API Gateway per-route burst capacity for public Tenant GitHub webhook ingress."
  type        = number
  default     = 100

  validation {
    condition     = var.tenant_github_webhook_throttle_burst > 0 && var.tenant_github_webhook_throttle_burst <= 5000
    error_message = "Tenant GitHub Webhook throttle burst must be between 1 and 5000 requests."
  }
}

variable "tenant_github_webhook_reader_role_arns" {
  description = "Exact, operator-approved per-Tenant Webhook-purpose reader Role ARNs the API may assume. No private-key or OAuth reader roles are accepted by this contract."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for role_arn in var.tenant_github_webhook_reader_role_arns : can(regex("^arn:aws:iam::[0-9]{12}:role/hako/tenant-webhook-reader/[A-Za-z0-9_-]{1,128}$", role_arn))
    ]) && (!var.tenant_github_webhook_enabled || length(var.tenant_github_webhook_reader_role_arns) > 0)
    error_message = "Tenant Webhook ingress requires exact per-Tenant Webhook-purpose reader Role ARNs under role/hako/tenant-webhook-reader; wildcard and other-purpose roles are forbidden."
  }
}

variable "github_app_slug" {
  description = "GitHub App slug used to generate the tenant Installation setup URL."
  type        = string
  default     = ""
}

variable "github_app_id" {
  description = "Numeric GitHub App ID verified on the OAuth Installation callback."
  type        = number
  default     = 0
}

variable "github_app_client_id" {
  description = "OAuth client ID used to exchange the setup callback code."
  type        = string
  default     = ""
}

variable "github_app_client_secret_arn" {
  description = "Secrets Manager ARN containing the GitHub App OAuth client secret."
  type        = string
  default     = ""
}

variable "github_app_callback_url" {
  description = "HTTPS OAuth callback URL registered in the GitHub App settings."
  type        = string
  default     = ""
}

variable "github_app_setup_enabled" {
  description = "Enable the secure Tenant GitHub App Installation setup flow when all App OAuth settings are configured."
  type        = bool
  default     = false

  validation {
    condition = !var.github_app_setup_enabled || (
      can(regex("^[A-Za-z0-9-]{1,100}$", var.github_app_slug)) &&
      var.github_app_id > 0 &&
      length(trimspace(var.github_app_client_id)) > 0 &&
      can(regex("^https://[^?#]+$", var.github_app_callback_url)) &&
      can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.github_app_client_secret_arn))
    )
    error_message = "GitHub App setup requires a valid slug, positive App ID, OAuth client ID, HTTPS callback URL, and Secrets Manager client-secret ARN."
  }
}

variable "api_lambda_image_uri" {
  description = "Optional immutable same-Region ECR image URI (including @sha256 digest) used to create a blue/green API Lambda candidate. Empty omits the candidate."
  type        = string
  default     = ""

  validation {
    condition     = var.api_lambda_image_uri == "" || can(regex("^[0-9]{12}\\.dkr\\.ecr\\.[a-z0-9-]+\\.amazonaws\\.com\\/.+@sha256:[0-9a-f]{64}$", var.api_lambda_image_uri))
    error_message = "api_lambda_image_uri must be empty or a same-account ECR image URI pinned by sha256 digest."
  }
}

variable "api_lambda_image_active" {
  description = "Route API Gateway traffic to the prepared image Lambda candidate. Requires api_lambda_image_uri; defaults false so candidates can be tested before promotion."
  type        = bool
  default     = false

  validation {
    condition     = !var.api_lambda_image_active || trimspace(var.api_lambda_image_uri) != ""
    error_message = "api_lambda_image_active requires api_lambda_image_uri to identify a prepared image candidate."
  }
}

variable "dsql_deletion_protection_enabled" {
  description = "Protect the Control Plane DSQL cluster from accidental deletion."
  type        = bool
  default     = true
}

variable "default_workspace_image" {
  description = "Image identifier passed to newly created Workspaces."
  type        = string
  default     = "hako-dev:latest"
}

variable "default_workspace_runtime_class" {
  description = "Default Hako Runtime Class passed to the API Lambda."
  type        = string
  default     = "standard"
}

variable "api_lambda_memory_size" {
  description = "Memory allocated to the Control Plane API Lambda in MiB."
  type        = number
  default     = 1024

  validation {
    condition     = var.api_lambda_memory_size >= 128 && var.api_lambda_memory_size <= 10240
    error_message = "api_lambda_memory_size must be between 128 and 10240 MiB."
  }
}

variable "api_lambda_timeout_seconds" {
  description = "API Lambda timeout; must fit within the HTTP API integration timeout."
  type        = number
  default     = 25

  validation {
    condition     = var.api_lambda_timeout_seconds >= 1 && var.api_lambda_timeout_seconds <= 29 && floor(var.api_lambda_timeout_seconds) == var.api_lambda_timeout_seconds
    error_message = "api_lambda_timeout_seconds must be between 1 and 29 seconds."
  }
}

variable "lambda_log_retention_days" {
  description = "Retention period for API Lambda logs."
  type        = number
  default     = 30

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1827, 3653], var.lambda_log_retention_days)
    error_message = "lambda_log_retention_days must be a supported CloudWatch Logs retention value."
  }
}

variable "tags" {
  description = "Additional tags applied to taggable Control Plane resources; Hako ownership tags are reserved."
  type        = map(string)
  default     = {}
}

variable "enable_outbox_dispatcher" {
  description = "Opt in to the scheduled Outbox Dispatcher Lambda after Resource Plane queue setup is ready."
  type        = bool
  default     = false
}

variable "enable_github_checks_delivery" {
  description = "Provision the dedicated encrypted GitHub Checks queue/DLQ, Dispatcher send access, consumer policy, and queue alarms."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_github_checks_delivery || var.enable_outbox_dispatcher
    error_message = "GitHub Checks delivery requires the Outbox Dispatcher to be enabled."
  }
}

variable "github_checks_queue_visibility_timeout_seconds" {
  description = "Visibility timeout for the dedicated GitHub Checks queue; must be between 1 and 43200 seconds."
  type        = number
  default     = 900

  validation {
    condition     = var.github_checks_queue_visibility_timeout_seconds >= 1 && var.github_checks_queue_visibility_timeout_seconds <= 43200 && floor(var.github_checks_queue_visibility_timeout_seconds) == var.github_checks_queue_visibility_timeout_seconds
    error_message = "github_checks_queue_visibility_timeout_seconds must be a whole number from 1 to 43200."
  }
}

variable "github_checks_queue_age_alarm_threshold_seconds" {
  description = "Alert when the oldest visible GitHub Checks message exceeds this age for two periods."
  type        = number
  default     = 300

  validation {
    condition     = var.github_checks_queue_age_alarm_threshold_seconds >= 60 && floor(var.github_checks_queue_age_alarm_threshold_seconds) == var.github_checks_queue_age_alarm_threshold_seconds
    error_message = "github_checks_queue_age_alarm_threshold_seconds must be a whole number of at least 60."
  }
}

variable "github_checks_queue_depth_alarm_threshold" {
  description = "Alert when visible GitHub Checks messages exceed this queue depth for two periods."
  type        = number
  default     = 100

  validation {
    condition     = var.github_checks_queue_depth_alarm_threshold >= 0 && floor(var.github_checks_queue_depth_alarm_threshold) == var.github_checks_queue_depth_alarm_threshold
    error_message = "github_checks_queue_depth_alarm_threshold must be a non-negative whole number."
  }
}

variable "github_checks_alarm_actions" {
  description = "Optional SNS topic ARNs notified by GitHub Checks queue age, depth, and DLQ alarms."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for arn in var.github_checks_alarm_actions : can(regex("^arn:[^:]+:sns:[^:]+:[0-9]{12}:.+$", arn))])
    error_message = "github_checks_alarm_actions entries must be SNS topic ARNs."
  }
}

variable "enable_github_checks_consumer_service" {
  description = "Deploy the long-running GitHub Checks consumer as an ECS/Fargate service. Requires explicit private-network, digest-pinned image, broker URL, and mTLS Secret inputs."
  type        = bool
  default     = false

  validation {
    condition = !var.enable_github_checks_consumer_service || (
      var.enable_github_checks_delivery &&
      can(regex("^arn:[^:]+:ecs:[^:]+:[0-9]{12}:cluster/.+$", var.github_checks_consumer_cluster_arn)) &&
      length(var.github_checks_consumer_subnet_ids) > 0 &&
      length(var.github_checks_consumer_security_group_ids) > 0 &&
      can(regex("^([0-9]{12})\\.dkr\\.ecr\\.([a-z0-9-]+)\\.amazonaws\\.com/(.+)@sha256:[0-9a-f]{64}$", var.github_checks_consumer_image_uri)) &&
      try(
        split(":", var.github_checks_consumer_ecr_repository_arn)[0] == "arn" &&
        split(":", var.github_checks_consumer_ecr_repository_arn)[2] == "ecr" &&
        split(":", var.github_checks_consumer_ecr_repository_arn)[3] == regex("^([0-9]{12})\\.dkr\\.ecr\\.([a-z0-9-]+)\\.amazonaws\\.com/(.+)@sha256:[0-9a-f]{64}$", var.github_checks_consumer_image_uri)[1] &&
        split(":", var.github_checks_consumer_ecr_repository_arn)[4] == regex("^([0-9]{12})\\.dkr\\.ecr\\.([a-z0-9-]+)\\.amazonaws\\.com/(.+)@sha256:[0-9a-f]{64}$", var.github_checks_consumer_image_uri)[0] &&
        split(":", var.github_checks_consumer_ecr_repository_arn)[5] == "repository/${regex("^([0-9]{12})\\.dkr\\.ecr\\.([a-z0-9-]+)\\.amazonaws\\.com/(.+)@sha256:[0-9a-f]{64}$", var.github_checks_consumer_image_uri)[2]}",
        false
      ) &&
      can(regex("^https://[^?#]+$", var.github_checks_broker_url)) &&
      var.github_checks_broker_client_cert_secret_arn != "" &&
      var.github_checks_broker_client_key_secret_arn != "" &&
      var.github_checks_broker_ca_secret_arn != ""
    )
    error_message = "The Checks consumer service requires Checks delivery, an ECS cluster, private subnets/security groups, a digest-pinned image, HTTPS broker URL, and three mTLS Secret ARNs."
  }
}

variable "github_checks_consumer_cluster_arn" {
  description = "Pre-created ECS cluster ARN used only when the Checks Fargate service is enabled."
  type        = string
  default     = ""
}

variable "github_checks_consumer_subnet_ids" {
  description = "Private subnet IDs for the Checks Fargate service; public IP assignment is always disabled."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.github_checks_consumer_subnet_ids : can(regex("^subnet-[A-Za-z0-9]+$", id))])
    error_message = "github_checks_consumer_subnet_ids must contain subnet IDs."
  }
}

variable "github_checks_consumer_security_group_ids" {
  description = "Pre-created security groups for the Checks consumer's restricted outbound broker/AWS/API access."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.github_checks_consumer_security_group_ids : can(regex("^sg-[A-Za-z0-9]+$", id))])
    error_message = "github_checks_consumer_security_group_ids must contain security group IDs."
  }
}

variable "github_checks_consumer_image_uri" {
  description = "ARM64 Checks consumer image URI pinned by sha256 digest."
  type        = string
  default     = ""
}

variable "github_checks_consumer_ecr_repository_arn" {
  description = "Exact private ECR repository ARN from which the Fargate execution role may pull the Checks consumer image."
  type        = string
  default     = ""
}

variable "github_checks_broker_url" {
  description = "HTTPS URL for the private/mTLS GitHub Checks credential broker."
  type        = string
  default     = ""
}

variable "github_checks_broker_client_cert_secret_arn" {
  description = "Secrets Manager ARN containing the PEM client certificate injected only into the Checks consumer task."
  type        = string
  default     = ""

  validation {
    condition     = var.github_checks_broker_client_cert_secret_arn == "" || can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.github_checks_broker_client_cert_secret_arn))
    error_message = "github_checks_broker_client_cert_secret_arn must be empty or a Secrets Manager secret ARN."
  }
}

variable "github_checks_broker_client_key_secret_arn" {
  description = "Secrets Manager ARN containing the PEM private key injected only into the Checks consumer task."
  type        = string
  default     = ""

  validation {
    condition     = var.github_checks_broker_client_key_secret_arn == "" || can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.github_checks_broker_client_key_secret_arn))
    error_message = "github_checks_broker_client_key_secret_arn must be empty or a Secrets Manager secret ARN."
  }
}

variable "github_checks_broker_ca_secret_arn" {
  description = "Secrets Manager ARN containing the broker CA PEM injected only into the Checks consumer task."
  type        = string
  default     = ""

  validation {
    condition     = var.github_checks_broker_ca_secret_arn == "" || can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.github_checks_broker_ca_secret_arn))
    error_message = "github_checks_broker_ca_secret_arn must be empty or a Secrets Manager secret ARN."
  }
}

variable "github_checks_broker_kms_key_arn" {
  description = "Optional customer-managed KMS key ARN used to encrypt the mTLS Secrets."
  type        = string
  default     = ""

  validation {
    condition     = var.github_checks_broker_kms_key_arn == "" || can(regex("^arn:[^:]+:kms:[^:]+:[0-9]{12}:key/.+$", var.github_checks_broker_kms_key_arn))
    error_message = "github_checks_broker_kms_key_arn must be empty or a KMS key ARN."
  }
}

variable "github_checks_consumer_desired_count" {
  description = "Desired number of stateless Fargate Checks consumers."
  type        = number
  default     = 1

  validation {
    condition     = var.github_checks_consumer_desired_count >= 1 && var.github_checks_consumer_desired_count <= 10 && floor(var.github_checks_consumer_desired_count) == var.github_checks_consumer_desired_count
    error_message = "github_checks_consumer_desired_count must be a whole number between 1 and 10."
  }
}

variable "github_checks_consumer_cpu" {
  description = "Fargate task CPU units for the Checks consumer."
  type        = number
  default     = 512

  validation {
    condition = contains([256, 512, 1024, 2048, 4096], var.github_checks_consumer_cpu) && (
      var.github_checks_consumer_cpu == 256 ? contains([512, 1024, 2048], var.github_checks_consumer_memory) :
      var.github_checks_consumer_cpu == 512 ? var.github_checks_consumer_memory >= 1024 && var.github_checks_consumer_memory <= 4096 && var.github_checks_consumer_memory % 1024 == 0 :
      var.github_checks_consumer_cpu == 1024 ? var.github_checks_consumer_memory >= 2048 && var.github_checks_consumer_memory <= 8192 && var.github_checks_consumer_memory % 1024 == 0 :
      var.github_checks_consumer_cpu == 2048 ? var.github_checks_consumer_memory >= 4096 && var.github_checks_consumer_memory <= 16384 && var.github_checks_consumer_memory % 1024 == 0 :
      var.github_checks_consumer_memory >= 8192 && var.github_checks_consumer_memory <= 30720 && var.github_checks_consumer_memory % 1024 == 0
    )
    error_message = "Checks consumer CPU/memory must be a valid Fargate task size combination."
  }
}

variable "github_checks_consumer_memory" {
  description = "Fargate task memory in MiB for the Checks consumer."
  type        = number
  default     = 1024
}

variable "github_checks_dsql_database" {
  description = "Aurora DSQL database name used by the Checks consumer."
  type        = string
  default     = "postgres"
}

variable "enable_workload_run_creation" {
  description = "Expose Workload Run creation. Enable only after the Workload scheduler consumer and runtime are deployed."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_workload_run_creation || var.enable_outbox_dispatcher
    error_message = "Workload Run creation requires the Outbox Dispatcher and scheduler queue to be enabled."
  }
}

variable "enable_webhook_workload_triggers" {
  description = "Opt in to per-repository push/pull_request verification Jobs; requires the webhook processor, Workload Runs, Checks delivery, and Dispatcher."
  type        = bool
  default     = false

  validation {
    condition = !var.enable_webhook_workload_triggers || (
      var.enable_github_webhook_processor &&
      var.enable_workload_run_creation &&
      var.enable_github_checks_delivery &&
      var.enable_outbox_dispatcher
    )
    error_message = "Webhook Workload triggers require the webhook processor, Workload Run creation, GitHub Checks delivery, and Outbox Dispatcher."
  }
}

variable "dispatcher_lambda_zip_path" {
  description = "Path to the arm64 custom-runtime ZIP produced by make build-dispatcher-lambda."
  type        = string
  default     = "../../../../../build/hako-dispatcher.zip"

  validation {
    condition     = !var.enable_outbox_dispatcher || fileexists(var.dispatcher_lambda_zip_path)
    error_message = "Build the Outbox Dispatcher Lambda zip before enabling it."
  }
}

variable "enable_github_webhook_processor" {
  description = "Explicitly enable the scheduled processor for GitHub Installation repository events."
  type        = bool
  default     = false
}

variable "github_webhook_processor_lambda_zip_path" {
  description = "Path to the arm64 custom-runtime ZIP generated by make build-webhook-processor-lambda."
  type        = string
  default     = "../../../../../build/hako-webhook-processor.zip"

  validation {
    condition     = !var.enable_github_webhook_processor || fileexists(var.github_webhook_processor_lambda_zip_path)
    error_message = "Build the GitHub Webhook Processor Lambda zip before enabling it."
  }
}

variable "resource_plane_manifest_json" {
  description = "Versioned Resource Plane manifest JSON embedded in the optional Dispatcher Lambda environment."
  type        = string
  default     = ""

  validation {
    condition = !var.enable_outbox_dispatcher || (
      length(var.resource_plane_manifest_json) > 0 &&
      length(var.resource_plane_manifest_json) <= 2500 &&
      can(jsondecode(var.resource_plane_manifest_json).schema_version == 1) &&
      can(length(jsondecode(var.resource_plane_manifest_json).resource_planes) > 0)
    )
    error_message = "When enabled, the Dispatcher needs a valid non-empty version-1 manifest no larger than 2500 bytes."
  }
}

variable "cognito_issuer_url" {
  description = "Exact Cognito User Pool issuer URL, including the user pool ID path."
  type        = string

  validation {
    condition     = can(regex("^https://[^/]+/.+", var.cognito_issuer_url))
    error_message = "cognito_issuer_url must be the HTTPS Cognito issuer URL including the user pool path."
  }
}

variable "cognito_user_pool_id" {
  description = "ID of the existing Cognito User Pool where the Hako API resource server is managed."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9-]+_[A-Za-z0-9]+$", var.cognito_user_pool_id))
    error_message = "cognito_user_pool_id must be a Cognito User Pool ID such as ap-northeast-1_Example123."
  }
}

variable "cognito_cli_client_name" {
  description = "Name of the public Cognito app client created for the Hako CLI."
  type        = string
  default     = "hako-cli"
}

variable "cognito_cli_callback_urls" {
  description = "OAuth callback URLs registered on the public Hako CLI app client."
  type        = list(string)
  default     = ["http://127.0.0.1:53682/callback"]

  validation {
    condition     = length(var.cognito_cli_callback_urls) > 0 && alltrue([for callback in var.cognito_cli_callback_urls : can(regex("^https://", callback)) || can(regex("^http://(127\\.0\\.0\\.1|localhost)(:[0-9]+)?/", callback))])
    error_message = "Callback URLs must use HTTPS or HTTP loopback URLs."
  }
}
