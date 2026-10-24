mock_provider "aws" {}

run "empty_allowlist_creates_no_policy" {
  command = plan

  assert {
    condition     = length(aws_iam_role_policy.tenant_private_key_readers) == 0
    error_message = "an empty Tenant allowlist must not attach broker permissions"
  }
  assert {
    condition     = length(aws_iam_role_policy.tenant_model_credential_readers) == 0
    error_message = "an empty Tenant model credential allowlist must not attach broker permissions"
  }
}

run "private_key_allowlist_is_exact_and_broker_only" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_private_key_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
      "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-b",
    ]
    tenant_webhook_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader/tenant-a",
    ]
  }

  assert {
    condition     = length(aws_iam_role_policy.tenant_private_key_readers) == 1
    error_message = "configured Tenant private-key reader Roles must attach one broker policy"
  }
  assert {
    condition     = aws_iam_role_policy.tenant_private_key_readers[0].role == "hako-github-broker-prod"
    error_message = "private-key reader permissions must be attached only to the broker role"
  }
  assert {
    condition = (jsondecode(aws_iam_role_policy.tenant_private_key_readers[0].policy).Statement[0].Action == ["sts:AssumeRole"] &&
      toset(jsondecode(aws_iam_role_policy.tenant_private_key_readers[0].policy).Statement[0].Resource) == toset([
        "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
        "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-b",
    ]))
    error_message = "broker policy must allow only AssumeRole on the exact private-key reader ARN set"
  }
}

run "model_credential_allowlist_is_exact_and_separate" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_model_credential_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a",
      "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-b",
    ]
    tenant_private_key_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
    ]
    tenant_webhook_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader/tenant-a",
    ]
  }

  assert {
    condition     = length(aws_iam_role_policy.tenant_model_credential_readers) == 1
    error_message = "configured Tenant model credential reader Roles must attach one broker policy"
  }
  assert {
    condition     = aws_iam_role_policy.tenant_model_credential_readers[0].role == "hako-github-broker-prod"
    error_message = "model credential reader permissions must be attached only to the broker role"
  }
  assert {
    condition = (jsondecode(aws_iam_role_policy.tenant_model_credential_readers[0].policy).Statement[0].Action == ["sts:AssumeRole"] &&
      toset(jsondecode(aws_iam_role_policy.tenant_model_credential_readers[0].policy).Statement[0].Resource) == toset([
        "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a",
        "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-b",
    ]))
    error_message = "broker policy must allow only AssumeRole on the exact model credential reader ARN set"
  }
}

run "rejects_wildcard_and_wrong_purpose_role" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_private_key_reader_role_arns = [
      "arn:aws:iam::123456789012:role/*",
    ]
  }

  expect_failures = [var.tenant_private_key_reader_role_arns]
}

run "rejects_role_reuse_across_purposes" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_private_key_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
    ]
    tenant_webhook_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-private-key-reader/tenant-a",
    ]
  }

  expect_failures = [var.tenant_webhook_reader_role_arns]
}

run "rejects_model_credential_reader_role_reuse" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_model_credential_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a",
    ]
    tenant_webhook_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/tenant-a",
    ]
  }

  expect_failures = [var.tenant_model_credential_reader_role_arns]
}

run "rejects_wildcard_model_credential_role" {
  command = plan

  variables {
    broker_role_name = "hako-github-broker-prod"
    tenant_model_credential_reader_role_arns = [
      "arn:aws:iam::123456789012:role/hako/tenant-model-credential-reader/*",
    ]
  }

  expect_failures = [var.tenant_model_credential_reader_role_arns]
}
