mock_provider "aws" {
  alias = "mock"

  mock_data "aws_iam_policy_document" {
    defaults = {
      json          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
      minified_json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_region" {
    defaults = {
      name = "ap-northeast-1"
    }
  }

  mock_data "aws_route_table" {
    defaults = {
      id     = "rtb-hako-workload-mock"
      vpc_id = "vpc-hako-workload-mock"
      routes = [{
        carrier_gateway_id         = null
        cidr_block                 = "0.0.0.0/0"
        core_network_arn           = null
        destination_prefix_list_id = null
        egress_only_gateway_id     = null
        gateway_id                 = null
        instance_id                = null
        ipv6_cidr_block            = null
        local_gateway_id           = null
        nat_gateway_id             = "nat-hako-workload-mock"
        network_interface_id       = null
        transit_gateway_id         = null
        vpc_endpoint_id            = null
        vpc_peering_connection_id  = null
      }]
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

  mock_resource "aws_lb" {
    defaults = {
      arn      = "arn:aws:elasticloadbalancing:ap-northeast-1:123456789012:loadbalancer/app/rp-test-gateway/1234567890abcdef"
      dns_name = "rp-test-gateway-1234567890.ap-northeast-1.elb.amazonaws.com"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:ap-northeast-1:123456789012:targetgroup/rp-test-gateway/1234567890abcdef"
    }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::hako-artifact-bucket"
      id  = "hako-artifact-bucket"
    }
  }

  mock_resource "aws_s3_bucket_ownership_controls" {}
  mock_resource "aws_s3_bucket_public_access_block" {}
  mock_resource "aws_s3_bucket_versioning" {}
  mock_resource "aws_s3_bucket_server_side_encryption_configuration" {}
  mock_resource "aws_s3_bucket_lifecycle_configuration" {}
  mock_resource "aws_s3_bucket_policy" {}
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
    condition     = length(aws_lambda_event_source_mapping.workload_controller) == 0 && length(aws_cloudwatch_event_rule.workload_controller_recovery) == 0 && length(aws_cloudwatch_event_rule.workload_artifact_cleanup) == 0
    error_message = "Command, recovery, and artifact cleanup triggers must remain disabled by default."
  }

  assert {
    condition     = length(aws_iam_role_policy.workload_controller_runtime) == 0
    error_message = "Lambda MicroVM and S3 runtime IAM must remain disabled by default."
  }

  assert {
    condition = (length(aws_s3_bucket.workload_artifacts) == 0 &&
      length(aws_s3_bucket_policy.workload_artifacts) == 0 &&
    length(aws_s3_bucket_public_access_block.workload_artifacts) == 0)
    error_message = "Managed Workload artifact storage must remain disabled by default."
  }

  assert {
    condition     = length(aws_iam_role_policy.resource_controller_workspace_storage) == 0
    error_message = "Workspace EFS and dynamic execution-role IAM must remain disabled by default."
  }

  assert {
    condition = (length(aws_sqs_queue.actions_runner_startup) == 0 && length(aws_lambda_function.actions_runner_worker) == 0 &&
      length(aws_lambda_event_source_mapping.actions_runner_worker) == 0 && length(aws_lambda_function.actions_runner_cleanup) == 0 &&
    length(aws_cloudwatch_event_rule.actions_runner_cleanup) == 0)
    error_message = "Actions Runner queues, startup worker, cleanup worker, and schedules must remain disabled by default."
  }

  assert {
    condition = (length(aws_sqs_queue.workspace_service_commands) == 0 &&
      length(aws_lambda_function.workspace_service_controller) == 0 &&
      length(aws_lambda_event_source_mapping.workspace_service_controller) == 0 &&
    length(aws_iam_role_policy.workspace_service_controller) == 0)
    error_message = "Workspace Service queue, consumer, event mapping, and runtime permissions must remain disabled by default."
  }
}

run "workspace_storage_runtime_permissions_are_exact_and_opt_in" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    workspace_storage_runtime = {
      file_system_id  = "fs-0123456789abcdef0"
      file_system_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:file-system/fs-0123456789abcdef0"
    }
  }

  assert {
    condition = (length(aws_iam_role_policy.resource_controller_workspace_storage) == 1 &&
      anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_storage[0].policy).Statement :
        statement.Sid == "CreateWorkspaceAccessPointsOnExactFilesystem" &&
        statement.Action == ["elasticfilesystem:CreateAccessPoint"] &&
        statement.Resource == "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:file-system/fs-0123456789abcdef0" &&
        statement.Condition.StringEquals["aws:RequestTag/hako:resource-plane-id"] == "rp-test"
        ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_storage[0].policy).Statement :
        statement.Sid == "TagOnlyNewWorkspaceAccessPoints" &&
        statement.Condition.StringEquals["elasticfilesystem:CreateAction"] == "CreateAccessPoint"
    ]))
    error_message = "Access Point creation and tagging must be constrained to one configured EFS filesystem and this Resource Plane's ownership tags."
  }

  assert {
    condition = (anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_storage[0].policy).Statement :
      statement.Sid == "ManageOnlyTaggedWorkspaceExecutionRoles" &&
      statement.Resource == "arn:aws:iam::123456789012:role/hako/workspaces/*" &&
      statement.Condition.StringEquals["aws:ResourceTag/hako:resource-plane-id"] == "rp-test" &&
      !contains(keys(statement.Condition.StringEquals), "aws:RequestTag/hako:resource-plane-id") &&
      !contains(statement.Action, "iam:UpdateAssumeRolePolicy") &&
      !contains(statement.Action, "iam:AttachRolePolicy") &&
      !contains(statement.Action, "iam:PutRolePermissionsBoundary")
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_storage[0].policy).Statement :
      statement.Sid == "CreateOnlyTaggedWorkspaceExecutionRoles" &&
      statement.Action == ["iam:CreateRole"] &&
      statement.Condition.StringEquals["aws:RequestTag/hako:resource-plane-id"] == "rp-test"
    ]))
    error_message = "Role creation and existing-role operations must use the role path and the appropriate request/resource tag conditions without trust or managed-policy mutation."
  }

  assert {
    condition = (anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_storage[0].policy).Statement :
      statement.Sid == "PassOnlyWorkspaceExecutionRolesToLambda" &&
      statement.Action == ["iam:PassRole"] &&
      statement.Resource == "arn:aws:iam::123456789012:role/hako/workspaces/*" &&
      statement.Condition.StringEquals["iam:PassedToService"] == "lambda.amazonaws.com" &&
      !contains(keys(statement.Condition), "aws:ResourceTag")
    ]))
    error_message = "PassRole must be limited to Workspace roles and Lambda, without an unsupported resource-tag condition."
  }
}

run "workspace_lambda_microvm_controller_is_disabled_by_default" {
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
    condition = (length(aws_lambda_function.workspace_resource_controller) == 0 &&
      length(aws_lambda_event_source_mapping.workspace_resource_controller) == 0 &&
      length(aws_iam_role_policy.resource_controller_workspace_runtime) == 0 &&
      length(aws_sqs_queue.preview_runtime_commands) == 0 &&
      length(aws_lambda_event_source_mapping.preview_runtime_controller) == 0 &&
    output.workspace_resource_controller_lambda_arn == null)
    error_message = "Workspace and Preview Lambda MicroVM controllers and Preview queues must remain absent unless explicitly configured."
  }
}

run "workspace_lambda_microvm_controller_uses_scoped_runtime_iam" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                              = "rp-test"
    control_plane_dispatcher_role_arn              = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn         = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    control_plane_preview_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-preview-result-consumer"
    enable_preview_runtime                         = true
    workspace_storage_runtime = {
      file_system_id  = "fs-0123456789abcdef0"
      file_system_arn = "arn:aws:elasticfilesystem:ap-northeast-1:123456789012:file-system/fs-0123456789abcdef0"
    }
    workspace_runtime_deployment = {
      zip_path                       = "../../../../build/resource-controller-lambda.zip"
      journal_bucket_arn             = "arn:aws:s3:::hako-rp-journal"
      journal_bucket_name            = "hako-rp-journal"
      journal_prefix                 = "workspace-runtime/v1"
      runtime_class                  = "standard"
      microvm_image_arn              = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard"
      microvm_image_version          = "1"
      egress_network_connectors      = ["arn:aws:lambda:ap-northeast-1:123456789012:network-connector:workspace-egress"]
      maximum_duration_seconds       = 28800
      auto_resume_enabled            = true
      max_idle_duration_seconds      = 3600
      suspended_duration_seconds     = 0
      lambda_timeout_seconds         = 60
      operation_timeout_seconds      = 45
      lambda_memory_size             = 512
      reserved_concurrent_executions = 1
    }
  }

  assert {
    condition = (length(aws_lambda_function.workspace_resource_controller) == 1 &&
      length(aws_lambda_event_source_mapping.workspace_resource_controller) == 1 &&
      length(aws_lambda_event_source_mapping.preview_runtime_controller) == 1 &&
      length(aws_sqs_queue.preview_runtime_commands) == 1 &&
      length(aws_sqs_queue.preview_runtime_commands_dlq) == 1 &&
      length(aws_sqs_queue.preview_runtime_results) == 1 &&
      length(aws_sqs_queue.preview_runtime_results_dlq) == 1 &&
      length(aws_lambda_function.fake_resource_controller) == 0 &&
      contains(aws_lambda_function.workspace_resource_controller[0].architectures, "arm64") &&
      length(aws_lambda_function.workspace_resource_controller[0].architectures) == 1 &&
    aws_lambda_function.workspace_resource_controller[0].reserved_concurrent_executions == 1)
    error_message = "The production Workspace controller must be one ARM64 Lambda consumer and cannot share the queue with the Fake controller."
  }

  assert {
    condition = (anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_runtime[0].policy).Statement :
      statement.Sid == "OperateOnlyConfiguredWorkspaceMicroVMImage" &&
      statement.Resource == "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard" &&
      alltrue([for action in statement.Action : contains(["lambda:RunMicrovm", "lambda:GetMicrovm", "lambda:SuspendMicrovm", "lambda:ResumeMicrovm", "lambda:TerminateMicrovm"], action)]) &&
      length(statement.Action) == 5
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_runtime[0].policy).Statement :
      statement.Sid == "ReadWriteOnlyWorkspaceRuntimeJournalPrefix" &&
      statement.Resource == "arn:aws:s3:::hako-rp-journal/workspace-runtime/v1/*" &&
      statement.Action == ["s3:GetObject", "s3:PutObject"]
      ]) && alltrue([for statement in jsondecode(aws_iam_role_policy.resource_controller_workspace_runtime[0].policy).Statement :
      statement.Sid != "ListOnlyWorkspaceRuntimeJournalPrefix" &&
      !contains(statement.Action, "s3:ListBucket") &&
      !contains(statement.Action, "s3:DeleteObject")
    ]))
    error_message = "Runtime IAM must be limited to the configured image and exact Workspace journal objects without list/delete permissions."
  }

  assert {
    condition = (jsondecode(aws_lambda_function.workspace_resource_controller[0].environment[0].variables.HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON).image_identifier == "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard" &&
      jsondecode(aws_lambda_function.workspace_resource_controller[0].environment[0].variables.HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON).execution_role_arn == "arn:aws:iam::123456789012:role/hako/workspaces/profile-placeholder" &&
      contains(aws_lambda_event_source_mapping.workspace_resource_controller[0].function_response_types, "ReportBatchItemFailures") &&
    length(aws_lambda_event_source_mapping.workspace_resource_controller[0].function_response_types) == 1)
    error_message = "The Lambda environment must receive only the trusted static launch profile and use per-record SQS failure reporting."
  }

  assert {
    condition = (
      aws_lambda_function.workspace_resource_controller[0].environment[0].variables.HAKO_PREVIEW_RUNTIME_COMMAND_QUEUE_ARN == aws_sqs_queue.preview_runtime_commands[0].arn &&
      aws_lambda_function.workspace_resource_controller[0].environment[0].variables.HAKO_PREVIEW_RUNTIME_RESULT_QUEUE_URL == aws_sqs_queue.preview_runtime_results[0].url &&
      aws_lambda_event_source_mapping.preview_runtime_controller[0].event_source_arn == aws_sqs_queue.preview_runtime_commands[0].arn
    )
    error_message = "Preview queues must be mapped to the Resource Controller Lambda and passed through the exact environment settings."
  }

  assert {
    condition = (
      strcontains(aws_sqs_queue_policy.preview_runtime_commands[0].policy, "arn:aws:iam::123456789012:role/hako-cp-dispatcher") &&
      strcontains(aws_sqs_queue_policy.preview_runtime_results[0].policy, "arn:aws:iam::123456789012:role/hako-cp-preview-result-consumer")
    )
    error_message = "Preview command/result queues must be private to the exact dispatcher and result consumer."
  }

}

run "workspace_service_controller_isolated_and_opt_in" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    command_visibility_timeout_seconds     = 6000
    workspace_service_controller_vpc_config = {
      subnet_ids         = ["subnet-11111111", "subnet-22222222"]
      security_group_ids = ["sg-11111111"]
    }
    workspace_service_controller_deployment = {
      # Plan-only test input; `tofu test` never applies this file.
      zip_path                       = "workload-controller.tftest.hcl"
      journal_bucket_arn             = "arn:aws:s3:::hako-journal-bucket"
      journal_prefix                 = "v1/workspace-runtime"
      microvm_image_arn              = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-workspace"
      ticket_key_id                  = "rp-test-ticket-v1"
      ticket_key_secret_arn          = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/service-ticket-AbCdEf"
      report_key_secret_arn          = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/service-report-AbCdEf"
      report_endpoint                = "https://api.hako.example"
      lambda_timeout_seconds         = 300
      lambda_memory_size             = 1024
      reserved_concurrent_executions = 2
    }
  }

  assert {
    condition = (aws_sqs_queue.workspace_service_commands[0].sqs_managed_sse_enabled &&
      jsondecode(aws_sqs_queue.workspace_service_commands[0].redrive_policy).deadLetterTargetArn == aws_sqs_queue.workspace_service_commands_dlq[0].arn &&
      jsondecode(aws_sqs_queue_policy.workspace_service_commands[0].policy).Statement[0].Principal.AWS == "arn:aws:iam::123456789012:role/hako-cp-dispatcher" &&
    jsondecode(aws_sqs_queue_policy.workspace_service_commands[0].policy).Statement[0].Action == "sqs:SendMessage")
    error_message = "Workspace Service queue must be encrypted, use its own DLQ, and accept sends only from the exact Dispatcher role."
  }

  assert {
    condition = (aws_lambda_event_source_mapping.workspace_service_controller[0].event_source_arn == aws_sqs_queue.workspace_service_commands[0].arn &&
      aws_lambda_event_source_mapping.workspace_service_controller[0].batch_size == 1 &&
    contains(aws_lambda_event_source_mapping.workspace_service_controller[0].function_response_types, "ReportBatchItemFailures"))
    error_message = "Workspace Service consumer must process its dedicated queue one message at a time with partial failure reporting."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "ReadOnlyExactWorkspaceServiceSecrets" && length(statement.Resource) == 2 && !contains(statement.Resource, "*")
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "OperateOnlyConfiguredWorkspaceMicroVMImage" &&
      statement.Resource == "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-workspace" &&
    !contains(statement.Action, "lambda:ConnectMicrovm")])
    error_message = "Consumer IAM must use only the two configured Secrets and exact MicroVM image; VPC ConnectMicrovm permission is not granted."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "CreateOnlyTaggedWorkspaceServiceGeneratedSecrets" &&
      statement.Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/workspaces/*/services/*/*" &&
      contains(statement.Action, "secretsmanager:CreateSecret") &&
      statement.Condition.StringLike["secretsmanager:Name"] == "hako/workspaces/*/services/*/*" &&
      statement.Condition.StringEquals["aws:RequestTag/hako:managed-by"] == "hako"
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "ReadOnlyTaggedWorkspaceServiceGeneratedSecrets" &&
      statement.Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/workspaces/*/services/*/*" &&
      contains(statement.Action, "secretsmanager:DescribeSecret") && contains(statement.Action, "secretsmanager:GetSecretValue") &&
      statement.Condition.StringEquals["aws:ResourceTag/hako:managed-by"] == "hako"
    ])
    error_message = "Generated Service credentials must use Hako-path-scoped Secrets Manager access with constrained create tags and tagged reads."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "PutTaggedWorkspaceServiceCredentialVersions" &&
      statement.Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/workspaces/*/services/*/*" &&
      contains(statement.Action, "secretsmanager:PutSecretValue") &&
      statement.Condition.StringEquals["aws:ResourceTag/hako:managed-by"] == "hako"
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "MoveOnlyWorkspaceServiceCredentialStages" &&
      statement.Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/workspaces/*/services/*/*" &&
      contains(statement.Action, "secretsmanager:UpdateSecretVersionStage") &&
      statement.Condition.StringEquals["aws:ResourceTag/hako:managed-by"] == "hako" &&
      contains(statement.Condition.StringEquals["secretsmanager:VersionStage"], "AWSPENDING") &&
      contains(statement.Condition.StringEquals["secretsmanager:VersionStage"], "AWSCURRENT")
    ])
    error_message = "Credential rotation must write only tagged Hako Service Secrets Manager versions and controlled stages."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workspace_service_controller[0].policy).Statement :
      statement.Sid == "RetireOnlyTaggedWorkspaceServiceGeneratedSecrets" &&
      contains(statement.Action, "secretsmanager:DeleteSecret") &&
      statement.Resource == "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/workspaces/*/services/*/*" &&
      statement.Condition.StringEquals["aws:ResourceTag/hako:managed-by"] == "hako"
    ])
    error_message = "Service Secret retirement must be limited to Hako-owned generated credentials in the exact Service secret path."
  }

  assert {
    condition = (aws_cloudwatch_metric_alarm.workspace_service_commands_dlq_visible[0].threshold == 0 &&
      aws_cloudwatch_metric_alarm.workspace_service_commands_oldest_message[0].threshold == 300 &&
    aws_cloudwatch_metric_alarm.workspace_service_controller_errors[0].threshold == 0)
    error_message = "The opt-in Service consumer must include DLQ, queue age, and Lambda error alarms."
  }
}

run "actions_runner_worker_consumes_only_its_opt_in_queue" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    command_visibility_timeout_seconds     = 6000
    actions_runner_vpc_config = {
      subnet_ids         = ["subnet-11111111", "subnet-22222222"]
      security_group_ids = ["sg-11111111"]
    }
    actions_runner_deployment = {
      zip_path                       = "workload-controller.tftest.hcl"
      broker_url                     = "https://broker.hako.example"
      diagnostics_upload_base_url    = "https://api.hako.example"
      broker_client_cert_secret_arn  = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/runner-cert-AbCdEf"
      broker_client_key_secret_arn   = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/runner-key-AbCdEf"
      broker_ca_secret_arn           = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/runner-ca-AbCdEf"
      bootstrap_signing_key_arn      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/runner-signing-AbCdEf"
      microvm_image_arn              = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-actions-runner"
      microvm_image_version          = "2"
      execution_role_arn             = "arn:aws:iam::123456789012:role/hako-runner-microvm"
      egress_connector_arn           = "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-runner-egress"
      maximum_duration_seconds       = 28800
      startup_timeout_seconds        = 240
      lambda_timeout_seconds         = 300
      lambda_memory_size             = 2048
      reserved_concurrent_executions = 3
    }
    actions_runner_cleanup_deployment = {}
  }

  assert {
    condition = (aws_sqs_queue.actions_runner_startup[0].sqs_managed_sse_enabled &&
    jsondecode(aws_sqs_queue.actions_runner_startup[0].redrive_policy).deadLetterTargetArn == aws_sqs_queue.actions_runner_startup_dlq[0].arn)
    error_message = "Actions Runner startup messages must be encrypted and redriven only to their dedicated DLQ."
  }

  assert {
    condition = (jsondecode(aws_sqs_queue_policy.actions_runner_startup[0].policy).Statement[0].Principal.AWS == "arn:aws:iam::123456789012:role/hako-cp-dispatcher" &&
    jsondecode(aws_sqs_queue_policy.actions_runner_startup[0].policy).Statement[0].Action == "sqs:SendMessage")
    error_message = "Only the exact Control Plane Dispatcher role may publish startup messages."
  }

  assert {
    condition = (aws_lambda_event_source_mapping.actions_runner_worker[0].event_source_arn == aws_sqs_queue.actions_runner_startup[0].arn &&
      aws_lambda_event_source_mapping.actions_runner_worker[0].batch_size == 1 &&
    contains(aws_lambda_event_source_mapping.actions_runner_worker[0].function_response_types, "ReportBatchItemFailures"))
    error_message = "Runner worker must consume only its dedicated queue and return partial batch failures."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.actions_runner_worker[0].policy).Statement :
      statement.Sid == "ConsumeOnlyActionsRunnerStartupQueue" && statement.Resource == aws_sqs_queue.actions_runner_startup[0].arn
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.actions_runner_worker[0].policy).Statement :
      statement.Sid == "ReadOnlyExactActionsRunnerSecrets" && length(statement.Resource) == 4 && !contains(statement.Resource, "*")
    ])
    error_message = "Runner worker queue and Secret permissions must be explicitly scoped."
  }

  assert {
    condition     = aws_lambda_function.actions_runner_worker[0].environment[0].variables.HAKO_ACTIONS_RUNNER_DIAGNOSTICS_UPLOAD_BASE_URL == "https://api.hako.example"
    error_message = "Runner bootstrap must carry the HTTPS Control Plane diagnostics endpoint into the one-shot guest capability."
  }

  assert {
    condition = (aws_lambda_function.actions_runner_cleanup[0].timeout == 900 &&
      aws_lambda_function.actions_runner_cleanup[0].reserved_concurrent_executions == 1 &&
      aws_cloudwatch_event_rule.actions_runner_cleanup[0].schedule_expression == "rate(5 minutes)" &&
      aws_cloudwatch_event_target.actions_runner_cleanup[0].arn == aws_lambda_function.actions_runner_cleanup[0].arn &&
    aws_lambda_permission.actions_runner_cleanup_events[0].source_arn == aws_cloudwatch_event_rule.actions_runner_cleanup[0].arn)
    error_message = "Scheduled cleanup must target its single-concurrency, bounded Lambda under an exact EventBridge invoke grant."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.actions_runner_cleanup[0].policy).Statement :
      statement.Sid == "ReadOnlyExactActionsRunnerBrokerSecrets" && length(statement.Resource) == 3 &&
      !contains(statement.Resource, "*")
      ]) && anytrue([for statement in jsondecode(aws_iam_role_policy.actions_runner_cleanup[0].policy).Statement :
      statement.Sid == "TerminateOnlyPinnedRunnerImageMicroVMs" &&
      statement.Resource == "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-actions-runner" &&
      contains(statement.Action, "lambda:GetMicrovm") && contains(statement.Action, "lambda:TerminateMicrovm")
    ])
    error_message = "Cleanup IAM must read only the three broker Secrets and terminate MicroVMs only for the pinned runner image."
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
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/artifact-cleanup-hmac-AbCdEf",
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
    workload_artifact_storage = {
      retention_days = 90
    }
    workload_controller_vpc_config = {
      subnet_ids         = ["subnet-11111111", "subnet-22222222"]
      security_group_ids = ["sg-11111111"]
    }
    workload_controller_deployment = {
      # Plan-only test input; no Lambda package is applied by `tofu test`.
      zip_path                             = "workload-controller.tftest.hcl"
      control_plane_endpoint               = "https://api.hako.example"
      control_plane_hmac_secret_arn        = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/metadata-AbCdEf"
      artifact_cleanup_hmac_secret_arn     = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/artifact-cleanup-hmac-AbCdEf"
      broker_url                           = "https://broker.hako.example"
      broker_client_cert_secret_arn        = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-cert-AbCdEf"
      broker_client_key_secret_arn         = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-key-AbCdEf"
      broker_ca_secret_arn                 = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/broker-ca-AbCdEf"
      ticket_key_id                        = "controller-v1"
      ticket_private_key_secret_arn        = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/ticket-key-AbCdEf"
      runtime_profiles_json                = "[{\"runtime_class\":\"standard\",\"trust_level\":\"untrusted\",\"image_identifier\":\"arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard\",\"image_version\":\"1\",\"image_digest\":\"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"ingress_connector\":\"arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS\",\"egress_connector\":\"arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-egress\"}]"
      recovery_page_size                   = 40
      recovery_max_pages                   = 4
      recovery_lease_ttl                   = "5m"
      recovery_schedule_expression         = "rate(1 minute)"
      artifact_cleanup_batch_size          = 50
      artifact_cleanup_schedule_expression = "rate(1 day)"
      lambda_timeout_seconds               = 60
      lambda_memory_size                   = 1024
      reserved_concurrent_executions       = 2
    }
  }

  assert {
    condition     = aws_lambda_function.workload_controller[0].environment[0].variables["HAKO_WORKLOAD_LAMBDA_MODE"] == "combined"
    error_message = "Worker Lambda must use the combined command/recovery event handler."
  }

  assert {
    condition     = toset(aws_lambda_function.workload_controller[0].vpc_config[0].subnet_ids) == toset(["subnet-11111111", "subnet-22222222"]) && toset(aws_lambda_function.workload_controller[0].vpc_config[0].security_group_ids) == toset(["sg-11111111"])
    error_message = "Worker Lambda must attach to the explicitly supplied private subnets and security group."
  }

  assert {
    condition = alltrue([for route_table in values(data.aws_route_table.workload_controller_private_subnet) :
      anytrue([for route in route_table.routes :
        try(route.cidr_block == "0.0.0.0/0" && route.nat_gateway_id != "", false)
      ])
    ])
    error_message = "Worker deployment must plan against subnet route tables with a default NAT Gateway route."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workload_controller[0].policy).Statement :
      statement.Sid == "CreateLambdaVpcNetworkInterfaces" && toset(statement.Action) == toset(["ec2:CreateNetworkInterface"]) &&
      contains(statement.Resource, "arn:aws:ec2:ap-northeast-1:123456789012:subnet/subnet-11111111") &&
      contains(statement.Resource, "arn:aws:ec2:ap-northeast-1:123456789012:subnet/subnet-22222222") &&
      contains(statement.Resource, "arn:aws:ec2:ap-northeast-1:123456789012:security-group/sg-11111111") &&
      !contains(statement.Resource, "*")
    ])
    error_message = "ENI creation must be scoped to the exact configured subnets and security groups."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workload_controller[0].policy).Statement :
      statement.Sid == "ManageLambdaVpcNetworkInterfaces" &&
      toset(statement.Action) == toset(["ec2:AssignPrivateIpAddresses", "ec2:DeleteNetworkInterface", "ec2:UnassignPrivateIpAddresses"]) &&
      statement.Resource == ["arn:aws:ec2:ap-northeast-1:123456789012:network-interface/*"]
    ])
    error_message = "ENI lifecycle operations must be limited to network-interface resources."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workload_controller[0].policy).Statement :
      statement.Sid == "DescribeLambdaVpcNetworkInterfaces" && toset(statement.Action) == toset(["ec2:DescribeNetworkInterfaces"]) && statement.Resource == ["*"]
    ])
    error_message = "Only the read-only DescribeNetworkInterfaces action may retain Resource wildcard scope."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workload_controller[0].policy).Statement :
      statement.Sid == "ReadOnlyExplicitWorkloadControllerSecrets" &&
      toset(statement.Action) == toset(["secretsmanager:GetSecretValue"]) &&
      toset(statement.Resource) == toset(var.workload_controller_secret_arns) &&
      contains(statement.Resource, var.workload_controller_deployment.artifact_cleanup_hmac_secret_arn)
    ])
    error_message = "The Lambda may read only the six named runtime Secrets, including its separate cleanup HMAC key."
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
    condition = (aws_cloudwatch_event_rule.workload_artifact_cleanup[0].schedule_expression == "rate(1 day)" &&
      aws_lambda_permission.workload_artifact_cleanup[0].source_arn == aws_cloudwatch_event_rule.workload_artifact_cleanup[0].arn &&
      jsondecode(aws_cloudwatch_event_target.workload_artifact_cleanup[0].input).hako_task == "artifact-cleanup" &&
      aws_lambda_function.workload_controller[0].environment[0].variables["HAKO_WORKLOAD_ARTIFACT_CLEANUP_BATCH_SIZE"] == "50" &&
    aws_lambda_function.workload_controller[0].environment[0].variables["HAKO_WORKLOAD_ARTIFACT_CLEANUP_HMAC_SECRET_ARN"] == var.workload_controller_deployment.artifact_cleanup_hmac_secret_arn)
    error_message = "Artifact cleanup must use its own bounded schedule, task envelope, and configured batch size."
  }

  assert {
    condition = anytrue([for statement in jsondecode(aws_iam_role_policy.workload_controller_runtime[0].policy).Statement :
      statement.Sid == "AttemptLogWriteAndExpiredObjectCleanup" &&
      toset(statement.Action) == toset(["s3:PutObject", "s3:DeleteObject"]) &&
      statement.Resource == "arn:aws:s3:::hako-artifact-bucket/v1/artifacts/*"
    ])
    error_message = "Artifact deletion must remain limited to the configured artifact prefix."
  }

  assert {
    condition = (aws_s3_bucket.workload_artifacts[0].bucket == "hako-artifact-bucket" &&
      aws_s3_bucket_public_access_block.workload_artifacts[0].block_public_acls &&
      aws_s3_bucket_public_access_block.workload_artifacts[0].block_public_policy &&
      aws_s3_bucket_public_access_block.workload_artifacts[0].ignore_public_acls &&
    aws_s3_bucket_public_access_block.workload_artifacts[0].restrict_public_buckets)
    error_message = "Managed Workload artifact storage must use the configured bucket name and block every public access path."
  }

  assert {
    condition = (aws_s3_bucket_ownership_controls.workload_artifacts[0].rule[0].object_ownership == "BucketOwnerEnforced" &&
      aws_s3_bucket_versioning.workload_artifacts[0].versioning_configuration[0].status == "Enabled" &&
    one([for rule in aws_s3_bucket_server_side_encryption_configuration.workload_artifacts[0].rule : rule.apply_server_side_encryption_by_default[0].sse_algorithm]) == "AES256")
    error_message = "Artifact storage must enforce bucket ownership, versioning, and default SSE-S3 encryption."
  }

  assert {
    condition = (length(aws_s3_bucket_lifecycle_configuration.workload_artifacts[0].rule) == 2 &&
      anytrue([for rule in aws_s3_bucket_lifecycle_configuration.workload_artifacts[0].rule :
        rule.id == "hako-attempt-artifact-retention" && rule.filter[0].prefix == "v1/artifacts/" &&
        rule.expiration[0].days == 90 && rule.noncurrent_version_expiration[0].noncurrent_days == 90 &&
        rule.abort_incomplete_multipart_upload[0].days_after_initiation == 1
    ]))
    error_message = "Artifact lifecycle must expire current and old versions under the exact prefix and abort incomplete uploads."
  }

  assert {
    condition = (anytrue([for statement in jsondecode(aws_s3_bucket_policy.workload_artifacts[0].policy).Statement :
      statement.Sid == "DenyInsecureTransport" && statement.Effect == "Deny" &&
      statement.Condition.Bool["aws:SecureTransport"] == "false"
      ]) && anytrue([for statement in jsondecode(aws_s3_bucket_policy.workload_artifacts[0].policy).Statement :
      statement.Sid == "AllowControllerAttemptArtifactAccess" &&
      statement.Principal.AWS == aws_iam_role.workload_controller[0].arn &&
      toset(statement.Action) == toset(["s3:PutObject", "s3:DeleteObject"]) &&
      statement.Resource == "arn:aws:s3:::hako-artifact-bucket/v1/artifacts/*"
    ]))
    error_message = "The bucket policy must deny insecure transport and grant only the controller role on the configured artifact prefix."
  }

  assert {
    condition     = contains(output.monitoring_alarm_names, "rp-test-workload-controller-errors") && contains(output.monitoring_alarm_names, "rp-test-workload-recovery-failed-invocations") && contains(output.monitoring_alarm_names, "rp-test-workload-artifact-cleanup-failed-invocations")
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

run "workload_controller_rejects_duplicate_or_malformed_vpc_ids" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    workload_controller_vpc_config = {
      subnet_ids         = ["subnet-11111111", "subnet-11111111"]
      security_group_ids = ["sg-invalid"]
    }
  }

  expect_failures = [
    var.workload_controller_vpc_config,
  ]
}

run "workspace_gateway_is_disabled_by_default" {
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
    condition = (length(aws_ecs_service.workspace_gateway) == 0 &&
      length(aws_lb.workspace_gateway) == 0 &&
      length(aws_lb_listener.workspace_gateway_https) == 0 &&
    length(aws_cloudwatch_metric_alarm.workspace_gateway_unhealthy_targets) == 0)
    error_message = "Workspace Gateway networking and ECS deployment must remain absent by default."
  }

  assert {
    condition = (length(aws_iam_role.workspace_gateway_task) == 0 &&
      length(aws_iam_role_policy.workspace_gateway_task) == 0 &&
    output.workspace_gateway_url == null)
    error_message = "Workspace Gateway runtime IAM and URL output must remain absent unless explicitly configured."
  }
}

run "workspace_gateway_uses_private_tasks_exact_iam_and_tls_wss_only" {
  command = plan
  providers = {
    aws = aws.mock
  }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    workspace_gateway_deployment = {
      image_uri                 = "123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/hako/gateway@sha256:${join("", [for _ in range(64) : "a"])}"
      secret_loader_image_uri   = "public.ecr.aws/aws-cli/aws-cli@sha256:${join("", [for _ in range(64) : "b"])}"
      ecs_cluster_arn           = "arn:aws:ecs:ap-northeast-1:123456789012:cluster/hako-rp"
      ecs_cluster_name          = "hako-rp"
      vpc_id                    = "vpc-0123456789abcdef0"
      vpc_cidr_block            = "10.20.0.0/16"
      public_subnet_ids         = ["subnet-11111111", "subnet-22222222"]
      private_subnet_ids        = ["subnet-33333333", "subnet-44444444"]
      ingress_cidr_blocks       = ["198.51.100.0/24"]
      egress_cidr_blocks        = ["10.20.0.0/16", "0.0.0.0/0"]
      acm_certificate_arn       = "arn:aws:acm:ap-northeast-1:123456789012:certificate/12345678-abcd-1234-abcd-123456789012"
      microvm_image_arn         = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-standard"
      journal_bucket_name       = "hako-journal-bucket"
      journal_bucket_arn        = "arn:aws:s3:::hako-journal-bucket"
      journal_prefix            = "v1/journal"
      session_lookup_endpoint   = "https://api.hako.example"
      connection_lease_endpoint = "https://api.hako.example"
      public_origin             = "https://gateway.hako.example"
      allowed_origins           = ["https://ide.hako.example"]
      agent_ticket_key_id       = "agent-v1"
      secret_arns = {
        tls_certificate          = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/gateway-tls-cert-AbCdEf"
        tls_private_key          = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/gateway-tls-key-AbCdEf"
        session_public_keys      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/session-public-AbCdEf"
        session_lookup_hmac      = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/session-hmac-AbCdEf"
        connection_lease_hmac    = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/lease-hmac-AbCdEf"
        agent_ticket_private_key = "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/agent-key-AbCdEf"
      }
      kms_key_arns                  = []
      access_log_bucket             = ""
      task_cpu                      = 1024
      task_memory                   = 2048
      desired_count                 = 2
      min_capacity                  = 2
      max_capacity                  = 5
      max_connections               = 500
      max_connections_per_workspace = 10
      log_retention_days            = 30
      deletion_protection           = true
    }
  }

  assert {
    condition = (length(aws_ecs_service.workspace_gateway) == 1 &&
      length(aws_lb_listener.workspace_gateway_https) == 1 &&
      length(aws_cloudwatch_metric_alarm.workspace_gateway_unhealthy_targets) == 1 &&
    length(aws_cloudwatch_metric_alarm.workspace_gateway_alb_5xx) == 1)
    error_message = "Explicit Gateway configuration must create an ECS service behind HTTPS listener."
  }

  assert {
    condition = (aws_lb_listener.workspace_gateway_https[0].port == 443 &&
      aws_lb_listener.workspace_gateway_https[0].protocol == "HTTPS" &&
      aws_lb_target_group.workspace_gateway[0].protocol == "HTTPS" &&
      aws_ecs_service.workspace_gateway[0].network_configuration[0].assign_public_ip == false &&
      aws_lb.workspace_gateway[0].idle_timeout == 3600 &&
      length(aws_lb.workspace_gateway[0].name) <= 32 &&
    length(aws_lb_target_group.workspace_gateway[0].name) <= 32)
    error_message = "Only HTTPS/WSS may be public and Gateway tasks must stay in private subnets."
  }

  assert {
    condition = contains(
      jsondecode(aws_iam_role_policy.workspace_gateway_task[0].policy).Statement[2].Resource,
      "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-test/agent-key-AbCdEf"
    )
    error_message = "Gateway task IAM must limit Secret reads to the six exact operator-provisioned ARNs."
  }

  assert {
    condition = (jsondecode(aws_ecs_task_definition.workspace_gateway[0].container_definitions)[1].mountPoints[0].readOnly == true &&
    jsondecode(aws_ecs_task_definition.workspace_gateway[0].container_definitions)[0].mountPoints[0].readOnly == false)
    error_message = "Secret loader must write to an ephemeral shared volume and Gateway must mount it read-only."
  }
}
