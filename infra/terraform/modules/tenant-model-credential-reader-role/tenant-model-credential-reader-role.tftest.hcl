mock_provider "aws" {
  mock_data "aws_partition" {
    defaults = { partition = "aws", dns_suffix = "amazonaws.com" }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { name = "ap-northeast-1" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-demo" }
  }
}

run "trusts_only_the_exact_broker_and_reads_only_the_tenant_secret" {
  command = plan

  variables {
    tenant_id       = "tenant-demo"
    broker_role_arn = "arn:aws:iam::210987654321:role/hako-github-broker-prod"
    secret_arn      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/model-AbCdEf"
  }

  assert {
    condition     = aws_iam_role.tenant_model_credential_reader.path == "/hako/tenant-model-credential-reader/"
    error_message = "model credentials must use their dedicated purpose path"
  }
  assert {
    condition = (
      jsondecode(aws_iam_role.tenant_model_credential_reader.assume_role_policy).Statement[0].Principal.AWS == "arn:aws:iam::210987654321:role/hako-github-broker-prod" &&
      jsondecode(aws_iam_role.tenant_model_credential_reader.assume_role_policy).Statement[0].Action == ["sts:AssumeRole"]
    )
    error_message = "the target Role trust must name only the exact Broker Role"
  }
  assert {
    condition = (
      jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement[0].Action == ["secretsmanager:GetSecretValue"] &&
      jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement[0].Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/model-AbCdEf" &&
      length(jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement) == 1
    )
    error_message = "default-key policy must read only the exact Tenant Secret"
  }
}

run "customer_key_is_exact_and_context_scoped" {
  command = plan

  variables {
    tenant_id       = "tenant-demo"
    broker_role_arn = "arn:aws:iam::210987654321:role/hako-github-broker-prod"
    secret_arn      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/model-AbCdEf"
    kms_key_arn     = "arn:aws:kms:ap-northeast-1:123456789012:key/01234567-89ab-cdef-0123-456789abcdef"
  }

  assert {
    condition = (
      jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement[1].Action == ["kms:Decrypt"] &&
      jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement[1].Resource == "arn:aws:kms:ap-northeast-1:123456789012:key/01234567-89ab-cdef-0123-456789abcdef" &&
      jsondecode(aws_iam_role_policy.tenant_model_credential_reader.policy).Statement[1].Condition.StringEquals["kms:EncryptionContext:SecretARN"] == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/model-AbCdEf"
    )
    error_message = "KMS decrypt must be restricted to the exact key and Secret encryption context"
  }
}

run "rejects_wildcard_secret_arn" {
  command = plan
  variables {
    tenant_id       = "tenant-demo"
    broker_role_arn = "arn:aws:iam::210987654321:role/hako-github-broker-prod"
    secret_arn      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/*"
  }
  expect_failures = [var.secret_arn]
}

run "rejects_wrong_region_secret" {
  command = plan
  variables {
    tenant_id       = "tenant-demo"
    broker_role_arn = "arn:aws:iam::210987654321:role/hako-github-broker-prod"
    secret_arn      = "arn:aws:secretsmanager:us-east-1:123456789012:secret:hako/tenant-demo/model-AbCdEf"
  }
  expect_failures = [aws_iam_role.tenant_model_credential_reader]
}

run "rejects_wildcard_broker_principal" {
  command = plan
  variables {
    tenant_id       = "tenant-demo"
    broker_role_arn = "arn:aws:iam::210987654321:role/*"
    secret_arn      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/tenant-demo/model-AbCdEf"
  }
  expect_failures = [aws_iam_role.tenant_model_credential_reader]
}
