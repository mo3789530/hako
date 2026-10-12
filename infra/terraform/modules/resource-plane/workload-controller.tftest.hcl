mock_provider "aws" {
  alias = "mock"

  mock_data "aws_iam_policy_document" {
    defaults = {
      json          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
      minified_json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/rp-test-mock"
      id  = "rp-test-mock"
    }
  }

  mock_resource "aws_sqs_queue" {
    defaults = {
      arn = "arn:aws:sqs:ap-northeast-1:123456789012:rp-test-mock"
      url = "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-test-mock"
    }
  }

  mock_resource "aws_cloudwatch_event_rule" {
    defaults = {
      arn = "arn:aws:events:ap-northeast-1:123456789012:rule/rp-test-mock"
    }
  }

  mock_resource "aws_lambda_function" {
    defaults = {
      arn = "arn:aws:lambda:ap-northeast-1:123456789012:function:rp-test-workload-controller"
    }
  }
}

run "workload_controller_is_disabled_by_default" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
  }

  assert {
    condition     = length(aws_lambda_function.workload_controller) == 0
    error_message = "Worker Lambda must remain disabled without explicit deployment configuration."
  }

  assert {
    condition     = length(aws_lambda_event_source_mapping.workload_controller) == 0 && length(aws_cloudwatch_event_rule.workload_controller_recovery) == 0
    error_message = "Command and recovery triggers must remain disabled by default."
  }

  assert {
    condition     = length(aws_iam_role_policy.workload_controller_runtime) == 0
    error_message = "Lambda MicroVM and S3 runtime IAM must remain disabled by default."
  }
}

run "workload_controller_wires_only_dedicated_queue_and_recovery" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                               = "rp-test"
    control_plane_dispatcher_role_arn               = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn          = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    control_plane_workload_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-workload-result-consumer"
    enable_workload_result_queue                    = true
    command_visibility_timeout_seconds              = 6000
    workload_controller_secret_arns = [
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/metadata-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-cert-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-key-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-ca-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/ticket-key-AbCdEf",
    ]
    workload_controller_runtime = {
      microvm_image_arn         = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard"
      ingress_connector_arn     = "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS"
      egress_connector_arn      = "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-egress"
      journal_bucket_arn        = "arn:aws:s3:::hako-journal-bucket"
      journal_prefix            = "v1/journal"
      recovery_lease_object_arn = "arn:aws:s3:::hako-journal-bucket/v1/journal/recovery/lease.json"
      source_bucket_arn         = "arn:aws:s3:::hako-source-bucket"
      source_prefix             = "v1/source"
      artifact_bucket_arn       = "arn:aws:s3:::hako-artifact-bucket"
      artifact_prefix           = "v1/artifacts"
    }
    workload_controller_deployment = {
      # Plan-only test input; no Lambda package is applied by `tofu test`.
      zip_path                       = "workload-controller.tftest.hcl"
      control_plane_endpoint         = "https://api.hako.example"
      control_plane_hmac_secret_arn  = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/metadata-AbCdEf"
      broker_url                     = "https://broker.hako.example"
      broker_client_cert_secret_arn  = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-cert-AbCdEf"
      broker_client_key_secret_arn   = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-key-AbCdEf"
      broker_ca_secret_arn           = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-ca-AbCdEf"
      ticket_key_id                  = "controller-v1"
      ticket_private_key_secret_arn  = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/ticket-key-AbCdEf"
      runtime_profiles_json          = "[{\"runtime_class\":\"standard\",\"trust_level\":\"untrusted\",\"image_identifier\":\"arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard\",\"image_version\":\"1\",\"image_digest\":\"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"ingress_connector\":\"arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS\",\"egress_connector\":\"arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-egress\"}]"
      recovery_page_size             = 40
      recovery_max_pages             = 4
      recovery_lease_ttl             = "5m"
      recovery_schedule_expression   = "rate(1 minute)"
      lambda_timeout_seconds         = 60
      lambda_memory_size             = 1024
      reserved_concurrent_executions = 2
    }
  }

  assert {
    condition     = aws_lambda_function.workload_controller[0].environment[0].variables["HAKO_WORKLOAD_LAMBDA_MODE"] == "combined"
    error_message = "Worker Lambda must use the combined command/recovery event handler."
  }

  assert {
    condition     = aws_lambda_event_source_mapping.workload_controller[0].event_source_arn == aws_sqs_queue.workload_commands[0].arn && aws_lambda_event_source_mapping.workload_controller[0].batch_size == 1 && contains(aws_lambda_event_source_mapping.workload_controller[0].function_response_types, "ReportBatchItemFailures")
    error_message = "Worker Lambda must consume only the dedicated Workload queue with per-record failure reporting."
  }

  assert {
    condition     = aws_cloudwatch_event_rule.workload_controller_recovery[0].schedule_expression == "rate(1 minute)" && aws_lambda_permission.workload_controller_recovery[0].source_arn == aws_cloudwatch_event_rule.workload_controller_recovery[0].arn
    error_message = "Recovery must use the bounded configured schedule and scoped Lambda invoke permission."
  }

  assert {
    condition     = contains(output.monitoring_alarm_names, "rp-test-workload-controller-errors") && contains(output.monitoring_alarm_names, "rp-test-workload-recovery-failed-invocations")
    error_message = "Worker and recovery alarms must be exposed to operators."
  }
}

run "workload_controller_rejects_traversal_in_recovery_lease_key" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                               = "rp-test"
    control_plane_dispatcher_role_arn               = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn          = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    control_plane_workload_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-workload-result-consumer"
    enable_workload_result_queue                    = true
    command_visibility_timeout_seconds              = 6000
    workload_controller_secret_arns = [
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/metadata-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-cert-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-key-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-ca-AbCdEf",
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/ticket-key-AbCdEf",
    ]
    workload_controller_runtime = {
      microvm_image_arn         = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard"
      ingress_connector_arn     = "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS"
      egress_connector_arn      = "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-egress"
      journal_bucket_arn        = "arn:aws:s3:::hako-journal-bucket"
      journal_prefix            = "v1/journal"
      recovery_lease_object_arn = "arn:aws:s3:::hako-journal-bucket/v1/journal/recovery/../lease.json"
      source_bucket_arn         = "arn:aws:s3:::hako-source-bucket"
      source_prefix             = "v1/source"
      artifact_bucket_arn       = "arn:aws:s3:::hako-artifact-bucket"
      artifact_prefix           = "v1/artifacts"
    }
  }

  expect_failures = [
    var.workload_controller_runtime,
  ]
}
