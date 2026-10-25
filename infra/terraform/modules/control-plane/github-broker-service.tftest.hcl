mock_provider "aws" {
  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/terraform-test"
      user_id    = "AIDATEST"
    }
  }

  mock_resource "aws_dsql_cluster" {
    defaults = {
      arn        = "arn:aws:dsql:ap-northeast-1:123456789012:cluster/test"
      identifier = "test-cluster"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/hako-test"
      id  = "hako-test"
    }
  }

  mock_resource "aws_cognito_resource_server" {
    defaults = {
      scope_identifiers = ["hako/api"]
    }
  }

  mock_resource "aws_apigatewayv2_api" {
    defaults = {
      execution_arn = "arn:aws:execute-api:ap-northeast-1:123456789012:api-test"
    }
  }
}

run "github_broker_service_is_disabled_by_default" {
  command = plan

  variables {
    aws_region           = "ap-northeast-1"
    api_lambda_zip_path  = "variables.tf"
    cognito_issuer_url   = "https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example123"
    cognito_user_pool_id = "ap-northeast-1_Example123"
  }

  assert {
    condition     = length(aws_ecs_service.github_broker) == 0 && length(aws_iam_role.github_broker_service) == 0
    error_message = "The Broker service and its workload role must remain absent unless explicitly enabled."
  }
}

run "github_broker_service_uses_private_network_and_exact_secrets" {
  command = plan

  variables {
    aws_region                                 = "ap-northeast-1"
    api_lambda_zip_path                        = "variables.tf"
    cognito_issuer_url                         = "https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example123"
    cognito_user_pool_id                       = "ap-northeast-1_Example123"
    enable_github_broker_service               = true
    github_broker_role_name                    = "hako-github-broker-test"
    github_broker_private_url                  = "https://broker.internal.example"
    github_broker_cluster_arn                  = "arn:aws:ecs:ap-northeast-1:123456789012:cluster/hako"
    github_broker_subnet_ids                   = ["subnet-aaa111", "subnet-bbb222"]
    github_broker_security_group_ids           = ["sg-broker"]
    github_broker_api_subnet_ids               = ["subnet-ccc333", "subnet-ddd444"]
    github_broker_api_security_group_ids       = ["sg-api"]
    github_broker_tcp_target_group_arn         = "arn:aws:elasticloadbalancing:ap-northeast-1:123456789012:targetgroup/hako-broker/0123456789abcdef"
    github_broker_image_uri                    = "123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/hako-broker@sha256:${join("", [for _ in range(64) : "a"])}"
    github_broker_ecr_repository_arn           = "arn:aws:ecr:ap-northeast-1:123456789012:repository/hako-broker"
    github_broker_callback_url                 = "https://hako.example/github/callback"
    github_broker_tls_cert_secret_arn          = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/server-cert-AbCdEf"
    github_broker_tls_key_secret_arn           = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/server-key-AbCdEf"
    github_broker_client_ca_secret_arn         = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/client-ca-AbCdEf"
    github_broker_api_client_cert_secret_arn   = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/client-cert-AbCdEf"
    github_broker_api_client_key_secret_arn    = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/client-key-AbCdEf"
    github_broker_api_ca_secret_arn            = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/broker-ca-AbCdEf"
    github_broker_agent_proposal_bucket        = "artifact-bucket"
    github_broker_agent_proposal_prefix        = "agent-artifacts"
    tenant_agent_proposal_artifact_object_arns = ["arn:aws:s3:::artifact-bucket/agent-artifacts/*/*/*/agent-changes/*.zip"]
  }

  assert {
    condition     = aws_ecs_service.github_broker[0].network_configuration[0].assign_public_ip == false
    error_message = "The Broker task must not receive a public IP address."
  }

  assert {
    condition     = toset(aws_ecs_service.github_broker[0].network_configuration[0].subnets) == toset(["subnet-aaa111", "subnet-bbb222"])
    error_message = "The Broker must attach only to the configured private subnet IDs."
  }

  assert {
    condition     = aws_ecs_task_definition.github_broker[0].runtime_platform[0].cpu_architecture == "ARM64"
    error_message = "The Broker task must use the ARM64 image architecture."
  }

  assert {
    condition = toset([for secret in jsondecode(aws_ecs_task_definition.github_broker[0].container_definitions)[0].secrets : secret.valueFrom]) == toset([
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/server-cert-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/server-key-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/broker/client-ca-AbCdEf",
    ])
    error_message = "The task definition must contain only exact TLS Secret ARNs, never their values."
  }

  assert {
    condition     = toset(aws_lambda_function.api.vpc_config[0].subnet_ids) == toset(["subnet-ccc333", "subnet-ddd444"])
    error_message = "The API Lambda must use the configured private subnets when Broker connectivity is enabled."
  }

  assert {
    condition     = contains(values(aws_lambda_function.api.environment[0].variables), "https://broker.internal.example")
    error_message = "The API Lambda must receive the configured private Broker origin."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_role_policy.api_lambda.policy).Statement :
      statement.Sid == "ReadOnlyBrokerClientTLSSecrets" &&
      statement.Action == ["secretsmanager:GetSecretValue"] &&
      toset(statement.Resource) == toset([
        "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/client-cert-AbCdEf",
        "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/client-key-AbCdEf",
        "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/api/broker-ca-AbCdEf",
      ])
    ])
    error_message = "The API role must read only the three configured Broker client TLS Secrets."
  }

  assert {
    condition = anytrue([
      for environment in jsondecode(aws_ecs_task_definition.github_broker[0].container_definitions)[0].environment :
      environment.name == "HAKO_GITHUB_BROKER_AGENT_PROPOSAL_BUCKET" && environment.value == "artifact-bucket"
      ]) && anytrue([
      for environment in jsondecode(aws_ecs_task_definition.github_broker[0].container_definitions)[0].environment :
      environment.name == "HAKO_GITHUB_BROKER_AGENT_PROPOSAL_PREFIX" && environment.value == "agent-artifacts"
    ])
    error_message = "The Broker must receive the exact configured proposal bucket/prefix through its opt-in task environment."
  }
}

run "github_broker_service_requires_complete_deployment_contract" {
  command = plan

  variables {
    aws_region                   = "ap-northeast-1"
    api_lambda_zip_path          = "variables.tf"
    cognito_issuer_url           = "https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example123"
    cognito_user_pool_id         = "ap-northeast-1_Example123"
    enable_github_broker_service = true
  }

  expect_failures = [var.enable_github_broker_service]
}
