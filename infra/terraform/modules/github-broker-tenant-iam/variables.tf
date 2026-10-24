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
