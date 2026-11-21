variable "tenant_id" {
  description = "Stable Hako Tenant identifier used for the dedicated reader Role name and tags."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9][A-Za-z0-9_-]{0,56}$", var.tenant_id))
    error_message = "tenant_id must be a 1-57 character IAM-safe identifier."
  }
}

variable "broker_role_arn" {
  description = "Exact Hako GitHub Credential Broker Role ARN allowed to assume this Tenant reader Role."
  type        = string
}

variable "secret_arn" {
  description = "Exact existing Secrets Manager Secret ARN for this Tenant's model credential; Terraform never reads its value."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$", var.secret_arn)) && !strcontains(var.secret_arn, "*")
    error_message = "secret_arn must be one exact Secrets Manager Secret ARN without wildcards."
  }
}

variable "kms_key_arn" {
  description = "Optional exact customer-managed KMS key ARN used by secret_arn. Empty means the Secrets Manager AWS-managed key."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Additional tags for the Tenant model credential reader Role."
  type        = map(string)
  default     = {}
}
