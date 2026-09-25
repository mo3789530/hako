variable "aws_region" {
  description = "AWS Region for the Control Plane account."
  type        = string
  default     = "ap-northeast-1"
}

variable "cognito_issuer_url" {
  description = "Exact Cognito User Pool issuer URL, including the user pool ID path."
  type        = string

  validation {
    condition     = can(regex("^https://[^/]+/.+", var.cognito_issuer_url))
    error_message = "cognito_issuer_url must be the HTTPS Cognito issuer URL including the user pool path."
  }
}

variable "api_lambda_arn" {
  description = "ARN of the deployed Hako API Lambda function."
  type        = string
}

variable "cognito_user_pool_id" {
  description = "ID of the Cognito User Pool used by the Control Plane."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9-]+_[A-Za-z0-9]+$", var.cognito_user_pool_id))
    error_message = "cognito_user_pool_id must be a Cognito User Pool ID such as ap-northeast-1_Example123."
  }
}

variable "cognito_cli_callback_urls" {
  description = "OAuth callback URLs registered on the public Hako CLI app client."
  type        = list(string)
  default     = ["http://127.0.0.1:53682/callback"]
}
