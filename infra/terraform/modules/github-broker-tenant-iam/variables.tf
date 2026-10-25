variable "broker_role_name" {
  description = "Existing Control Plane IAM role used only by the Hako GitHub Credential Broker."
  type        = string
  default     = ""

  validation {
    condition     = var.broker_role_name == "" || can(regex("^hako-github-broker[A-Za-z0-9+=,.@_-]{0,48}$", var.broker_role_name))
    error_message = "broker_role_name must be empty or an exact hako-github-broker role name."
  }
}

variable "policy_name_prefix" {
  description = "Stable prefix for this inline role policy."
  type        = string
  default     = "hako-github-broker"
}

variable "tenant_private_key_reader_role_arns" {
  description = "Exact operator-approved Tenant App private-key-purpose reader Role ARNs."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for role_arn in var.tenant_private_key_reader_role_arns : can(regex("^arn:aws:iam::[0-9]{12}:role/hako/tenant-private-key-reader/[A-Za-z0-9_-]{1,128}$", role_arn))
    ]) && (length(var.tenant_private_key_reader_role_arns) == 0 || var.broker_role_name != "")
    error_message = "Private-key readers must use exact Tenant-specific role ARNs and require an exact Hako GitHub Broker role."
  }
}

variable "tenant_webhook_reader_role_arns" {
  description = "Webhook-purpose reader Roles granted to the API Lambda; these must never be shared with the credential broker."
  type        = set(string)
  default     = []

  validation {
    condition     = length(setintersection(var.tenant_private_key_reader_role_arns, var.tenant_webhook_reader_role_arns)) == 0
    error_message = "Webhook-purpose and private-key-purpose reader Roles must be distinct."
  }
}

variable "tenant_model_credential_reader_role_arns" {
  description = "Exact operator-approved Tenant model credential reader Role ARNs. These are separate from GitHub App private-key and Webhook readers."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for role_arn in var.tenant_model_credential_reader_role_arns : can(regex("^arn:aws:iam::[0-9]{12}:role/hako/tenant-model-credential-reader/[A-Za-z0-9_-]{1,128}$", role_arn))
    ]) && (length(var.tenant_model_credential_reader_role_arns) == 0 || var.broker_role_name != "")
    error_message = "Model credential readers must use exact Tenant-specific role ARNs and require an exact Hako GitHub Broker role."
  }

  validation {
    condition = length(setintersection(
      var.tenant_model_credential_reader_role_arns,
      setunion(var.tenant_private_key_reader_role_arns, var.tenant_webhook_reader_role_arns),
    )) == 0
    error_message = "Model credential reader Roles must be distinct from GitHub private-key and Webhook reader Roles."
  }
}

variable "agent_proposal_artifact_object_arns" {
  description = "Exact S3 object ARN patterns for attempt-scoped Agent proposal ZIPs: bucket/prefix/*/*/*/agent-changes/*.zip. No list, write, or delete permissions are granted."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for object_arn in var.agent_proposal_artifact_object_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):s3:::[a-z0-9.-]{3,63}/[A-Za-z0-9/_=-]+/\\*/\\*/\\*/agent-changes/\\*\\.zip$", object_arn)) &&
      !can(regex("\\.\\.|//", object_arn))
    ]) && (length(var.agent_proposal_artifact_object_arns) == 0 || var.broker_role_name != "")
    error_message = "Agent proposal S3 access requires exact bucket/prefix/*/*/*/agent-changes/* object ARNs and an exact Hako Broker role."
  }
}

variable "agent_proposal_artifact_kms_key_arns" {
  description = "Exact customer-managed KMS key ARNs used by configured proposal artifact buckets. Each key policy must also authorize the exact Broker role."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for key_arn in var.agent_proposal_artifact_kms_key_arns : can(regex("^arn:(aws|aws-us-gov|aws-cn):kms:[a-z0-9-]+:[0-9]{12}:key/[A-Fa-f0-9-]+$", key_arn))
    ]) && (length(var.agent_proposal_artifact_kms_key_arns) == 0 || var.broker_role_name != "")
    error_message = "Agent proposal artifact KMS access requires concrete KMS key ARNs and an exact Hako Broker role."
  }
}
