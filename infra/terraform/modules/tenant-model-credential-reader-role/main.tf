terraform {
  required_version = ">= 1.12.1"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 6.0"
    }
  }
}

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  role_name = "tenant-${var.tenant_id}"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TrustExactHakoModelCredentialBroker"
      Effect    = "Allow"
      Action    = ["sts:AssumeRole"]
      Principal = { AWS = var.broker_role_arn }
    }]
  })
  secret_policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [{
        Sid      = "ReadOnlyTenantModelCredentialSecret"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.secret_arn
      }],
      var.kms_key_arn == "" ? [] : [{
        Sid      = "DecryptTenantSecretOnlyThroughSecretsManager"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService"                  = "secretsmanager.${data.aws_region.current.name}.${data.aws_partition.current.dns_suffix}"
            "kms:EncryptionContext:SecretARN" = var.secret_arn
          }
        }
      }]
    )
  })
}

resource "aws_iam_role" "tenant_model_credential_reader" {
  name                 = local.role_name
  path                 = "/hako/tenant-model-credential-reader/"
  assume_role_policy   = local.assume_role_policy
  max_session_duration = 3600

  tags = merge(var.tags, {
    "hako:managed-by" = "hako"
    "hako:tenant-id"  = var.tenant_id
    "hako:purpose"    = "tenant-model-credential-reader"
  })

  lifecycle {
    precondition {
      condition     = can(regex("^arn:${data.aws_partition.current.partition}:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]+$", var.broker_role_arn)) && !strcontains(var.broker_role_arn, "*")
      error_message = "broker_role_arn must be one exact IAM Role ARN without wildcards."
    }
    precondition {
      condition = (
        split(":", var.secret_arn)[3] == data.aws_region.current.name &&
        split(":", var.secret_arn)[4] == data.aws_caller_identity.current.account_id
      )
      error_message = "secret_arn must identify a Secret in the target Role account and Region."
    }
    precondition {
      condition = var.kms_key_arn == "" || (
        can(regex("^arn:${data.aws_partition.current.partition}:kms:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:key/[A-Fa-f0-9-]{36}$", var.kms_key_arn))
      )
      error_message = "kms_key_arn must be empty or an exact KMS key ARN in the target account and Region."
    }
  }
}

resource "aws_iam_role_policy" "tenant_model_credential_reader" {
  name   = "hako-tenant-model-credential-secret"
  role   = aws_iam_role.tenant_model_credential_reader.id
  policy = local.secret_policy
}
