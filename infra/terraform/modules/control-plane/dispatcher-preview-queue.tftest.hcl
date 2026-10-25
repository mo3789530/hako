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

  mock_resource "aws_lambda_function" {
    defaults = {
      arn = "arn:aws:lambda:ap-northeast-1:123456789012:function:hako-test"
    }
  }

  mock_resource "aws_cloudwatch_event_rule" {
    defaults = {
      arn = "arn:aws:events:ap-northeast-1:123456789012:rule/hako-test"
    }
  }

  mock_resource "aws_apigatewayv2_api" {
    defaults = {
      execution_arn = "arn:aws:execute-api:ap-northeast-1:123456789012:api-test"
    }
  }
}

run "dispatcher_can_publish_only_to_registered_preview_queue" {
  command = plan

  variables {
    aws_region                 = "ap-northeast-1"
    api_lambda_zip_path        = "variables.tf"
    dispatcher_lambda_zip_path = "variables.tf"
    enable_outbox_dispatcher   = true
    cognito_issuer_url         = "https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example123"
    cognito_user_pool_id       = "ap-northeast-1_Example123"
    resource_plane_manifest_json = jsonencode({
      schema_version = 1
      resource_planes = [{
        id                                = "rp-tokyo-01"
        provider                          = "aws"
        account_id                        = "123456789012"
        region                            = "ap-northeast-1"
        capabilities                      = ["microvm"]
        command_queue_url                 = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands"
        result_queue_url                  = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"
        preview_runtime_command_queue_url = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-preview-commands"
      }]
    })
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_role_policy.outbox_dispatcher.policy).Statement :
      statement.Sid == "PublishPreviewRuntimeCommandsToRegisteredPlanes" &&
      statement.Action == ["sqs:SendMessage"] &&
      statement.Resource == ["arn:aws:sqs:ap-northeast-1:123456789012:rp-tokyo-01-preview-commands"]
    ])
    error_message = "The Dispatcher role must receive SendMessage only for the Preview queue ARN registered to the Plane."
  }
}

run "dispatcher_rejects_preview_queue_from_another_plane_region" {
  command = plan

  variables {
    aws_region                 = "ap-northeast-1"
    api_lambda_zip_path        = "variables.tf"
    dispatcher_lambda_zip_path = "variables.tf"
    enable_outbox_dispatcher   = true
    cognito_issuer_url         = "https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_Example123"
    cognito_user_pool_id       = "ap-northeast-1_Example123"
    resource_plane_manifest_json = jsonencode({
      schema_version = 1
      resource_planes = [{
        id                                = "rp-tokyo-01"
        provider                          = "aws"
        account_id                        = "123456789012"
        region                            = "ap-northeast-1"
        capabilities                      = ["microvm"]
        command_queue_url                 = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-commands"
        result_queue_url                  = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-tokyo-01-results"
        preview_runtime_command_queue_url = "https://sqs.us-east-1.amazonaws.com/999999999999/foreign-preview-commands"
      }]
    })
  }

  expect_failures = [var.resource_plane_manifest_json]
}
