variable "api_name" {
  description = "Name of the Hako Control Plane HTTP API."
  type        = string
  default     = "hako-control-plane"
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
  description = "ARN of the Lambda function that runs cmd/hako-api in API Gateway Lambda mode."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:lambda:[^:]+:[0-9]{12}:function:.+", var.api_lambda_arn))
    error_message = "api_lambda_arn must be a Lambda function ARN."
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
