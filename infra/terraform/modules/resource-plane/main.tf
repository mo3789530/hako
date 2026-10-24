terraform {
  required_version = ">= 1.12.1"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 6.0"
    }
  }
}

locals {
  common_tags = merge(var.tags, {
    "hako:managed-by"        = "hako"
    "hako:resource-plane-id" = var.resource_plane_id
  })
}

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

resource "aws_sqs_queue" "commands" {
  name                       = "${var.resource_plane_id}-commands"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.command_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  tags = merge(local.common_tags, { "hako:queue-role" = "commands" })
}

resource "aws_sqs_queue" "commands_dlq" {
  name                      = "${var.resource_plane_id}-commands-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true

  tags = merge(local.common_tags, { "hako:queue-role" = "commands-dlq" })
}

resource "aws_sqs_queue_redrive_policy" "commands" {
  queue_url = aws_sqs_queue.commands.id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.commands_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "commands_dlq" {
  queue_url = aws_sqs_queue.commands_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.commands.arn]
  })
}

resource "aws_sqs_queue" "results" {
  name                       = "${var.resource_plane_id}-results"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.result_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  tags = merge(local.common_tags, { "hako:queue-role" = "results" })
}

resource "aws_sqs_queue" "results_dlq" {
  name                      = "${var.resource_plane_id}-results-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true

  tags = merge(local.common_tags, { "hako:queue-role" = "results-dlq" })
}

resource "aws_sqs_queue_redrive_policy" "results" {
  queue_url = aws_sqs_queue.results.id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.results_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "results_dlq" {
  queue_url = aws_sqs_queue.results_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.results.arn]
  })
}

# Preview runtime commands and acknowledgements have a distinct envelope and
# dispatcher/result-consumer trust boundary from Workspace Operations.
resource "aws_sqs_queue" "preview_runtime_commands" {
  count = var.enable_preview_runtime ? 1 : 0

  name                       = "${var.resource_plane_id}-preview-commands"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.command_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  tags                       = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-commands" })
}

resource "aws_sqs_queue" "preview_runtime_commands_dlq" {
  count = var.enable_preview_runtime ? 1 : 0

  name                      = "${var.resource_plane_id}-preview-commands-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-commands-dlq" })
}

resource "aws_sqs_queue_redrive_policy" "preview_runtime_commands" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_commands[0].id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.preview_runtime_commands_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "preview_runtime_commands_dlq" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_commands_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.preview_runtime_commands[0].arn]
  })
}

resource "aws_sqs_queue" "preview_runtime_results" {
  count = var.enable_preview_runtime ? 1 : 0

  name                       = "${var.resource_plane_id}-preview-results"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.result_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  tags                       = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-results" })
}

resource "aws_sqs_queue" "preview_runtime_results_dlq" {
  count = var.enable_preview_runtime ? 1 : 0

  name                      = "${var.resource_plane_id}-preview-results-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-results-dlq" })
}

resource "aws_sqs_queue_redrive_policy" "preview_runtime_results" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_results[0].id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.preview_runtime_results_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "preview_runtime_results_dlq" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_results_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.preview_runtime_results[0].arn]
  })
}

# Workload Run results have a different envelope and consumer from the
# Workspace Operation results above; keep their storage and IAM boundary
# separate and opt-in until the production worker/consumer is deployed.
resource "aws_sqs_queue" "workload_results" {
  count = var.enable_workload_result_queue ? 1 : 0

  name                       = "${var.resource_plane_id}-workload-results"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.result_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  tags = merge(local.common_tags, { "hako:queue-role" = "workload-results" })
}

resource "aws_sqs_queue" "workload_results_dlq" {
  count = var.enable_workload_result_queue ? 1 : 0

  name                      = "${var.resource_plane_id}-workload-results-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true

  tags = merge(local.common_tags, { "hako:queue-role" = "workload-results-dlq" })
}

resource "aws_sqs_queue_redrive_policy" "workload_results" {
  count = var.enable_workload_result_queue ? 1 : 0

  queue_url = aws_sqs_queue.workload_results[0].id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.workload_results_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "workload_results_dlq" {
  count = var.enable_workload_result_queue ? 1 : 0

  queue_url = aws_sqs_queue.workload_results_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.workload_results[0].arn]
  })
}

resource "aws_sqs_queue" "workload_commands" {
  count = var.enable_workload_result_queue ? 1 : 0

  name                       = "${var.resource_plane_id}-workload-commands"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.command_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.workload_commands_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
  tags = merge(local.common_tags, { "hako:queue-role" = "workload-commands" })
}

resource "aws_sqs_queue" "workload_commands_dlq" {
  count = var.enable_workload_result_queue ? 1 : 0

  name                      = "${var.resource_plane_id}-workload-commands-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "workload-commands-dlq" })
}

resource "aws_sqs_queue_redrive_allow_policy" "workload_commands_dlq" {
  count = var.enable_workload_result_queue ? 1 : 0

  queue_url = aws_sqs_queue.workload_commands_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.workload_commands[0].arn]
  })
}

# The Actions Runner startup path is independently opt-in. It receives only
# opaque job/attempt/claim keys; repository metadata and runner credentials are
# resolved by the Control Plane broker after mTLS authentication.
resource "aws_sqs_queue" "actions_runner_startup" {
  count = var.actions_runner_deployment == null ? 0 : 1

  name                       = "${var.resource_plane_id}-actions-runner-startup"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.command_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.actions_runner_startup_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
  tags = merge(local.common_tags, { "hako:queue-role" = "actions-runner-startup" })
}

resource "aws_sqs_queue" "actions_runner_startup_dlq" {
  count = var.actions_runner_deployment == null ? 0 : 1

  name                      = "${var.resource_plane_id}-actions-runner-startup-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "actions-runner-startup-dlq" })
}

resource "aws_sqs_queue_redrive_allow_policy" "actions_runner_startup_dlq" {
  count = var.actions_runner_deployment == null ? 0 : 1

  queue_url = aws_sqs_queue.actions_runner_startup_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.actions_runner_startup[0].arn]
  })
}

resource "aws_sqs_queue_policy" "actions_runner_startup" {
  count = var.actions_runner_deployment == null ? 0 : 1

  queue_url = aws_sqs_queue.actions_runner_startup[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlaneDispatcherSend"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_dispatcher_role_arn }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.actions_runner_startup[0].arn
    }]
  })
}

# The Workspace Service consumer is opt-in and uses a distinct queue and IAM
# role. Its Lambda stays in caller-provisioned private subnets; Terraform does
# not create the VPC endpoints or routes needed to reach the MicroVM/Control Plane.
resource "aws_sqs_queue" "workspace_service_commands" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  name                       = "${var.resource_plane_id}-workspace-services"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.command_visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.workspace_service_commands_dlq[0].arn
    maxReceiveCount     = var.max_receive_count
  })
  tags = merge(local.common_tags, { "hako:queue-role" = "workspace-service-commands" })
}

resource "aws_sqs_queue" "workspace_service_commands_dlq" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  name                      = "${var.resource_plane_id}-workspace-services-dlq"
  message_retention_seconds = var.dlq_retention_seconds
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "workspace-service-commands-dlq" })
}

resource "aws_sqs_queue_redrive_allow_policy" "workspace_service_commands_dlq" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  queue_url = aws_sqs_queue.workspace_service_commands_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.workspace_service_commands[0].arn]
  })
}

resource "aws_sqs_queue_policy" "workspace_service_commands" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  queue_url = aws_sqs_queue.workspace_service_commands[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlaneDispatcherSend"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_dispatcher_role_arn }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.workspace_service_commands[0].arn
    }]
  })
}

resource "aws_cloudwatch_log_group" "workspace_service_controller" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  name              = "/aws/lambda/${var.resource_plane_id}-workspace-service-controller"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "workspace-service-controller" })
}

data "aws_iam_policy_document" "workspace_service_controller_assume_role" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-workspace-service-controller"]
    }
  }
}

resource "aws_iam_role" "workspace_service_controller" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  name               = "${var.resource_plane_id}-workspace-service-controller"
  assume_role_policy = data.aws_iam_policy_document.workspace_service_controller_assume_role[0].json
  tags               = merge(local.common_tags, { "hako:component" = "workspace-service-controller" })
}

resource "aws_iam_role_policy" "workspace_service_controller" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-workspace-service-controller-runtime"
  role = aws_iam_role.workspace_service_controller[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ConsumeOnlyWorkspaceServiceCommands"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.workspace_service_commands[0].arn
      },
      {
        Sid      = "ReadOnlyWorkspaceRuntimeJournal"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.workspace_service_controller_deployment.journal_bucket_arn}/${var.workspace_service_controller_deployment.journal_prefix == "" ? "" : "${var.workspace_service_controller_deployment.journal_prefix}/"}${var.resource_plane_id}/*"
      },
      {
        Sid      = "ReadOnlyExactWorkspaceServiceSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = [var.workspace_service_controller_deployment.ticket_key_secret_arn, var.workspace_service_controller_deployment.report_key_secret_arn]
      },
      {
        Sid      = "CreateOnlyTaggedWorkspaceServiceGeneratedSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:CreateSecret"]
        Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:hako/workspaces/*/services/*/*"
        Condition = {
          StringLike = {
            "secretsmanager:Name"              = "hako/workspaces/*/services/*/*"
            "aws:RequestTag/hako:tenant-id"    = "?*"
            "aws:RequestTag/hako:workspace-id" = "?*"
            "aws:RequestTag/hako:service-id"   = "?*"
            "aws:RequestTag/hako:secret-name"  = "?*"
          }
          StringEquals = {
            "aws:RequestTag/hako:managed-by" = "hako"
          }
          "ForAllValues:StringEquals" = {
            "aws:TagKeys" = [
              "hako:managed-by",
              "hako:tenant-id",
              "hako:workspace-id",
              "hako:service-id",
              "hako:secret-name"
            ]
          }
        }
      },
      {
        Sid      = "ReadOnlyTaggedWorkspaceServiceGeneratedSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue"]
        Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:hako/workspaces/*/services/*/*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by" = "hako"
          }
        }
      },
      {
        Sid      = "PutTaggedWorkspaceServiceCredentialVersions"
        Effect   = "Allow"
        Action   = ["secretsmanager:PutSecretValue"]
        Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:hako/workspaces/*/services/*/*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by" = "hako"
          }
        }
      },
      {
        Sid      = "MoveOnlyWorkspaceServiceCredentialStages"
        Effect   = "Allow"
        Action   = ["secretsmanager:UpdateSecretVersionStage"]
        Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:hako/workspaces/*/services/*/*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by" = "hako"
            "secretsmanager:VersionStage"     = ["AWSCURRENT", "AWSPENDING"]
          }
        }
      },
      {
        Sid      = "RetireOnlyTaggedWorkspaceServiceGeneratedSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:DeleteSecret"]
        Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:hako/workspaces/*/services/*/*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by" = "hako"
          }
        }
      },
      {
        Sid      = "OperateOnlyConfiguredWorkspaceMicroVMImage"
        Effect   = "Allow"
        Action   = ["lambda:GetMicrovm", "lambda:CreateMicrovmAuthToken"]
        Resource = var.workspace_service_controller_deployment.microvm_image_arn
      },
      {
        Sid    = "CreateLambdaVpcNetworkInterfaces"
        Effect = "Allow"
        Action = ["ec2:CreateNetworkInterface"]
        Resource = concat(
          ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"],
          [for id in var.workspace_service_controller_vpc_config.subnet_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${id}"],
          [for id in var.workspace_service_controller_vpc_config.security_group_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:security-group/${id}"]
        )
      },
      {
        Sid      = "ManageLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:AssignPrivateIpAddresses", "ec2:DeleteNetworkInterface", "ec2:UnassignPrivateIpAddresses"]
        Resource = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"]
      },
      {
        Sid      = "DescribeLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:DescribeNetworkInterfaces"]
        Resource = ["*"]
      },
      {
        Sid      = "WriteOnlyWorkspaceServiceControllerLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.workspace_service_controller[0].arn}:*"
      }
      ], length(var.workspace_service_controller_deployment.secret_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyWorkspaceServiceSecrets"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.workspace_service_controller_deployment.secret_kms_key_arns
    }])
  })
}

resource "aws_lambda_function" "workspace_service_controller" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-workspace-service-controller"
  role                           = aws_iam_role.workspace_service_controller[0].arn
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  filename                       = var.workspace_service_controller_deployment.zip_path
  source_code_hash               = filebase64sha256(var.workspace_service_controller_deployment.zip_path)
  memory_size                    = var.workspace_service_controller_deployment.lambda_memory_size
  timeout                        = var.workspace_service_controller_deployment.lambda_timeout_seconds
  reserved_concurrent_executions = var.workspace_service_controller_deployment.reserved_concurrent_executions

  environment {
    variables = {
      HAKO_RESOURCE_PLANE_ID                       = var.resource_plane_id
      HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET        = trimprefix(var.workspace_service_controller_deployment.journal_bucket_arn, "arn:${data.aws_partition.current.partition}:s3:::")
      HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX        = var.workspace_service_controller_deployment.journal_prefix
      HAKO_WORKSPACE_SERVICE_TICKET_KEY_ID         = var.workspace_service_controller_deployment.ticket_key_id
      HAKO_WORKSPACE_SERVICE_TICKET_KEY_SECRET_ARN = var.workspace_service_controller_deployment.ticket_key_secret_arn
      HAKO_WORKSPACE_SERVICE_REPORT_KEY_SECRET_ARN = var.workspace_service_controller_deployment.report_key_secret_arn
      HAKO_WORKSPACE_SERVICE_REPORT_ENDPOINT       = var.workspace_service_controller_deployment.report_endpoint
    }
  }

  vpc_config {
    subnet_ids         = var.workspace_service_controller_vpc_config.subnet_ids
    security_group_ids = var.workspace_service_controller_vpc_config.security_group_ids
  }

  depends_on = [aws_iam_role_policy.workspace_service_controller]
  tags       = merge(local.common_tags, { "hako:component" = "workspace-service-controller" })

  lifecycle {
    precondition {
      condition     = split(":", var.workspace_service_controller_deployment.microvm_image_arn)[3] == data.aws_region.current.name
      error_message = "Workspace MicroVM image and service controller must be in the same AWS Region."
    }
    precondition {
      condition = alltrue([
        for arn in [var.workspace_service_controller_deployment.ticket_key_secret_arn, var.workspace_service_controller_deployment.report_key_secret_arn] :
        split(":", arn)[3] == data.aws_region.current.name
      ])
      error_message = "Workspace Service Secrets and controller must be in the same AWS Region."
    }
  }
}

resource "aws_lambda_event_source_mapping" "workspace_service_controller" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  event_source_arn        = aws_sqs_queue.workspace_service_commands[0].arn
  function_name           = aws_lambda_function.workspace_service_controller[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true
}

resource "aws_cloudwatch_log_group" "actions_runner_worker" {
  count = var.actions_runner_deployment == null ? 0 : 1

  name              = "/aws/lambda/${var.resource_plane_id}-actions-runner-worker"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "actions-runner-worker" })
}

data "aws_iam_policy_document" "actions_runner_worker_assume_role" {
  count = var.actions_runner_deployment == null ? 0 : 1

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-actions-runner-worker"]
    }
  }
}

resource "aws_iam_role" "actions_runner_worker" {
  count = var.actions_runner_deployment == null ? 0 : 1

  name               = "${var.resource_plane_id}-actions-runner-worker"
  assume_role_policy = data.aws_iam_policy_document.actions_runner_worker_assume_role[0].json
  tags               = merge(local.common_tags, { "hako:component" = "actions-runner-worker" })
}

resource "aws_iam_role_policy" "actions_runner_worker" {
  count = var.actions_runner_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-actions-runner-worker"
  role = aws_iam_role.actions_runner_worker[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ConsumeOnlyActionsRunnerStartupQueue"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.actions_runner_startup[0].arn
      },
      {
        Sid    = "ReadOnlyExactActionsRunnerSecrets"
        Effect = "Allow"
        Action = ["secretsmanager:GetSecretValue"]
        Resource = [
          var.actions_runner_deployment.broker_client_cert_secret_arn,
          var.actions_runner_deployment.broker_client_key_secret_arn,
          var.actions_runner_deployment.broker_ca_secret_arn,
          var.actions_runner_deployment.bootstrap_signing_key_arn
        ]
      },
      {
        Sid      = "OperateOnlyConfiguredLambdaMicroVMImage"
        Effect   = "Allow"
        Action   = ["lambda:RunMicrovm", "lambda:CreateMicrovmAuthToken"]
        Resource = var.actions_runner_deployment.microvm_image_arn
      },
      {
        Sid      = "PassOnlyConfiguredMicroVMNetworkConnector"
        Effect   = "Allow"
        Action   = ["lambda:PassNetworkConnector"]
        Resource = var.actions_runner_deployment.egress_connector_arn
      },
      {
        Sid      = "ConnectLambdaMicroVM"
        Effect   = "Allow"
        Action   = ["lambda:ConnectMicrovm"]
        Resource = "*"
      },
      {
        Sid      = "WriteOnlyActionsRunnerWorkerLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.actions_runner_worker[0].arn}:*"
      }
      ], var.actions_runner_deployment.execution_role_arn == "" ? [] : [{
        Sid      = "PassOnlyConfiguredMicroVMExecutionRole"
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = var.actions_runner_deployment.execution_role_arn
        Condition = {
          StringEquals = { "iam:PassedToService" = "lambda.amazonaws.com" }
        }
        }], length(var.actions_runner_deployment.secret_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyConfiguredRunnerSecretKeys"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.actions_runner_deployment.secret_kms_key_arns
        }], [{
        Sid    = "CreateLambdaVpcNetworkInterfaces"
        Effect = "Allow"
        Action = ["ec2:CreateNetworkInterface"]
        Resource = concat(
          ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"],
          [for id in var.actions_runner_vpc_config.subnet_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${id}"],
          [for id in var.actions_runner_vpc_config.security_group_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:security-group/${id}"]
        )
        }, {
        Sid      = "ManageLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:AssignPrivateIpAddresses", "ec2:DeleteNetworkInterface", "ec2:UnassignPrivateIpAddresses"]
        Resource = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"]
        }, {
        Sid      = "DescribeLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:DescribeNetworkInterfaces"]
        Resource = ["*"]
    }])
  })
}

resource "aws_lambda_function" "actions_runner_worker" {
  count = var.actions_runner_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-actions-runner-worker"
  role                           = aws_iam_role.actions_runner_worker[0].arn
  runtime                        = "provided.al2023"
  handler                        = "bootstrap"
  architectures                  = ["arm64"]
  filename                       = var.actions_runner_deployment.zip_path
  source_code_hash               = filebase64sha256(var.actions_runner_deployment.zip_path)
  timeout                        = var.actions_runner_deployment.lambda_timeout_seconds
  memory_size                    = var.actions_runner_deployment.lambda_memory_size
  reserved_concurrent_executions = var.actions_runner_deployment.reserved_concurrent_executions

  environment {
    variables = {
      HAKO_RESOURCE_PLANE_ID                               = var.resource_plane_id
      HAKO_ACTIONS_RUNNER_BROKER_URL                       = var.actions_runner_deployment.broker_url
      HAKO_ACTIONS_RUNNER_DIAGNOSTICS_UPLOAD_BASE_URL      = var.actions_runner_deployment.diagnostics_upload_base_url
      HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN    = var.actions_runner_deployment.broker_client_cert_secret_arn
      HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN     = var.actions_runner_deployment.broker_client_key_secret_arn
      HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN             = var.actions_runner_deployment.broker_ca_secret_arn
      HAKO_ACTIONS_RUNNER_BOOTSTRAP_SIGNING_KEY_SECRET_ARN = var.actions_runner_deployment.bootstrap_signing_key_arn
      HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_ARN                = var.actions_runner_deployment.microvm_image_arn
      HAKO_ACTIONS_RUNNER_MICROVM_IMAGE_VERSION            = var.actions_runner_deployment.microvm_image_version
      HAKO_ACTIONS_RUNNER_EXECUTION_ROLE_ARN               = var.actions_runner_deployment.execution_role_arn
      HAKO_ACTIONS_RUNNER_EGRESS_CONNECTOR_ARN             = var.actions_runner_deployment.egress_connector_arn
      HAKO_ACTIONS_RUNNER_MAXIMUM_DURATION_SECONDS         = tostring(var.actions_runner_deployment.maximum_duration_seconds)
      HAKO_ACTIONS_RUNNER_STARTUP_TIMEOUT                  = "${var.actions_runner_deployment.startup_timeout_seconds}s"
    }
  }

  vpc_config {
    subnet_ids         = var.actions_runner_vpc_config.subnet_ids
    security_group_ids = var.actions_runner_vpc_config.security_group_ids
  }

  lifecycle {
    precondition {
      condition = (split(":", var.actions_runner_deployment.microvm_image_arn)[3] == data.aws_region.current.name &&
        split(":", var.actions_runner_deployment.microvm_image_arn)[4] == data.aws_caller_identity.current.account_id &&
        split(":", var.actions_runner_deployment.egress_connector_arn)[3] == data.aws_region.current.name &&
        split(":", var.actions_runner_deployment.egress_connector_arn)[4] == data.aws_caller_identity.current.account_id &&
        alltrue([for arn in [var.actions_runner_deployment.broker_client_cert_secret_arn, var.actions_runner_deployment.broker_client_key_secret_arn,
          var.actions_runner_deployment.broker_ca_secret_arn, var.actions_runner_deployment.bootstrap_signing_key_arn] :
        split(":", arn)[3] == data.aws_region.current.name && split(":", arn)[4] == data.aws_caller_identity.current.account_id]) &&
        (var.actions_runner_deployment.execution_role_arn == "" || split(":", var.actions_runner_deployment.execution_role_arn)[4] == data.aws_caller_identity.current.account_id) &&
      alltrue([for arn in var.actions_runner_deployment.secret_kms_key_arns : split(":", arn)[3] == data.aws_region.current.name && split(":", arn)[4] == data.aws_caller_identity.current.account_id]))
      error_message = "Actions Runner MicroVM image, egress connector, runtime Secrets, KMS keys, and optional execution role must remain in this Resource Plane account and Region."
    }
  }

  depends_on = [aws_iam_role_policy.actions_runner_worker]
  tags       = merge(local.common_tags, { "hako:component" = "actions-runner-worker" })
}

resource "aws_lambda_event_source_mapping" "actions_runner_worker" {
  count = var.actions_runner_deployment == null ? 0 : 1

  event_source_arn        = aws_sqs_queue.actions_runner_startup[0].arn
  function_name           = aws_lambda_function.actions_runner_worker[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true
}

# Terminal runner cleanup is a separately opt-in scheduled function with a
# narrower role than the startup worker. It only reads the three mTLS Secrets,
# calls the Broker, and terminates MicroVMs created from the pinned runner image.
resource "aws_cloudwatch_log_group" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  name              = "/aws/lambda/${var.resource_plane_id}-actions-runner-cleanup"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "actions-runner-cleanup" })
}

data "aws_iam_policy_document" "actions_runner_cleanup_assume_role" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-actions-runner-cleanup"]
    }
  }
}

resource "aws_iam_role" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  name               = "${var.resource_plane_id}-actions-runner-cleanup"
  assume_role_policy = data.aws_iam_policy_document.actions_runner_cleanup_assume_role[0].json
  tags               = merge(local.common_tags, { "hako:component" = "actions-runner-cleanup" })
}

resource "aws_iam_role_policy" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-actions-runner-cleanup"
  role = aws_iam_role.actions_runner_cleanup[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid    = "ReadOnlyExactActionsRunnerBrokerSecrets"
        Effect = "Allow"
        Action = ["secretsmanager:GetSecretValue"]
        Resource = [
          var.actions_runner_deployment.broker_client_cert_secret_arn,
          var.actions_runner_deployment.broker_client_key_secret_arn,
          var.actions_runner_deployment.broker_ca_secret_arn
        ]
      },
      {
        Sid      = "TerminateOnlyPinnedRunnerImageMicroVMs"
        Effect   = "Allow"
        Action   = ["lambda:GetMicrovm", "lambda:TerminateMicrovm"]
        Resource = var.actions_runner_deployment.microvm_image_arn
      },
      {
        Sid      = "WriteOnlyActionsRunnerCleanupLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.actions_runner_cleanup[0].arn}:*"
      },
      {
        Sid    = "CreateLambdaVpcNetworkInterfaces"
        Effect = "Allow"
        Action = ["ec2:CreateNetworkInterface"]
        Resource = concat(
          ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"],
          [for id in var.actions_runner_vpc_config.subnet_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${id}"],
          [for id in var.actions_runner_vpc_config.security_group_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_caller_identity.current.account_id}:security-group/${id}"]
        )
      },
      {
        Sid      = "ManageLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:AssignPrivateIpAddresses", "ec2:DeleteNetworkInterface", "ec2:UnassignPrivateIpAddresses"]
        Resource = ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"]
      },
      {
        Sid      = "DescribeLambdaVpcNetworkInterfaces"
        Effect   = "Allow"
        Action   = ["ec2:DescribeNetworkInterfaces"]
        Resource = "*"
      }
      ], length(var.actions_runner_cleanup_deployment.secret_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyConfiguredRunnerBrokerSecretKeys"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.actions_runner_cleanup_deployment.secret_kms_key_arns
    }])
  })
}

resource "aws_lambda_function" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-actions-runner-cleanup"
  role                           = aws_iam_role.actions_runner_cleanup[0].arn
  runtime                        = "provided.al2023"
  handler                        = "bootstrap"
  architectures                  = ["arm64"]
  filename                       = var.actions_runner_deployment.zip_path
  source_code_hash               = filebase64sha256(var.actions_runner_deployment.zip_path)
  timeout                        = var.actions_runner_cleanup_deployment.lambda_timeout_seconds
  memory_size                    = var.actions_runner_cleanup_deployment.lambda_memory_size
  reserved_concurrent_executions = 1

  environment {
    variables = {
      HAKO_ACTIONS_RUNNER_MODE                          = "cleanup"
      HAKO_RESOURCE_PLANE_ID                            = var.resource_plane_id
      HAKO_ACTIONS_RUNNER_BROKER_URL                    = var.actions_runner_deployment.broker_url
      HAKO_ACTIONS_RUNNER_BROKER_CLIENT_CERT_SECRET_ARN = var.actions_runner_deployment.broker_client_cert_secret_arn
      HAKO_ACTIONS_RUNNER_BROKER_CLIENT_KEY_SECRET_ARN  = var.actions_runner_deployment.broker_client_key_secret_arn
      HAKO_ACTIONS_RUNNER_BROKER_CA_SECRET_ARN          = var.actions_runner_deployment.broker_ca_secret_arn
      HAKO_ACTIONS_RUNNER_CLEANUP_BATCH_SIZE            = tostring(var.actions_runner_cleanup_deployment.batch_size)
      HAKO_ACTIONS_RUNNER_CLEANUP_LEASE                 = "${var.actions_runner_cleanup_deployment.lease_duration_seconds}s"
      HAKO_ACTIONS_RUNNER_CLEANUP_OPERATION_TIMEOUT     = "${var.actions_runner_cleanup_deployment.operation_timeout_seconds}s"
      HAKO_ACTIONS_RUNNER_CLEANUP_POLL_INTERVAL         = "${var.actions_runner_cleanup_deployment.poll_interval_milliseconds}ms"
      HAKO_ACTIONS_RUNNER_CLEANUP_POLL_TIMEOUT          = "${var.actions_runner_cleanup_deployment.poll_timeout_seconds}s"
    }
  }

  vpc_config {
    subnet_ids         = var.actions_runner_vpc_config.subnet_ids
    security_group_ids = var.actions_runner_vpc_config.security_group_ids
  }

  depends_on = [aws_iam_role_policy.actions_runner_cleanup]
  tags       = merge(local.common_tags, { "hako:component" = "actions-runner-cleanup" })
}

resource "aws_cloudwatch_event_rule" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  name                = "${var.resource_plane_id}-actions-runner-cleanup"
  description         = "Poll the mTLS Broker for durable Actions Runner cleanup leases."
  schedule_expression = var.actions_runner_cleanup_deployment.schedule_expression
  state               = "ENABLED"
  tags                = merge(local.common_tags, { "hako:component" = "actions-runner-cleanup" })
}

resource "aws_cloudwatch_event_target" "actions_runner_cleanup" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  rule      = aws_cloudwatch_event_rule.actions_runner_cleanup[0].name
  target_id = "actions-runner-cleanup-lambda"
  arn       = aws_lambda_function.actions_runner_cleanup[0].arn
  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "actions_runner_cleanup_events" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  statement_id   = "AllowActionsRunnerCleanupSchedule"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.actions_runner_cleanup[0].function_name
  principal      = "events.amazonaws.com"
  source_arn     = aws_cloudwatch_event_rule.actions_runner_cleanup[0].arn
  source_account = data.aws_caller_identity.current.account_id
}

resource "aws_cloudwatch_metric_alarm" "actions_runner_cleanup_errors" {
  count = var.actions_runner_cleanup_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-actions-runner-cleanup-errors"
  alarm_description   = "Scheduled Actions Runner cleanup Lambda invocation errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    FunctionName = aws_lambda_function.actions_runner_cleanup[0].function_name
  }
  alarm_actions = var.alarm_actions
  ok_actions    = var.alarm_actions
  tags          = merge(local.common_tags, { "hako:component" = "actions-runner-cleanup" })
}

resource "aws_cloudwatch_metric_alarm" "actions_runner_startup_dlq_visible" {
  count = var.actions_runner_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-actions-runner-startup-dlq-visible"
  alarm_description   = "Actions Runner startup records reached the DLQ; inspect the claim lease and broker history before redrive."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.actions_runner_startup_dlq[0].name }
  tags                = merge(local.common_tags, { "hako:component" = "actions-runner-worker" })
}

resource "aws_cloudwatch_metric_alarm" "workspace_service_commands_dlq_visible" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workspace-services-dlq-visible"
  alarm_description   = "Workspace Service commands reached the dead-letter queue."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    QueueName = aws_sqs_queue.workspace_service_commands_dlq[0].name
  }
  alarm_actions = var.alarm_actions
  tags          = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "workspace_service_commands_oldest_message" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workspace-services-oldest-message"
  alarm_description   = "Workspace Service command processing is delayed."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    QueueName = aws_sqs_queue.workspace_service_commands[0].name
  }
  alarm_actions = var.alarm_actions
  tags          = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "workspace_service_controller_errors" {
  count = var.workspace_service_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workspace-service-controller-errors"
  alarm_description   = "Workspace Service controller Lambda reported invocation errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    FunctionName = aws_lambda_function.workspace_service_controller[0].function_name
  }
  alarm_actions = var.alarm_actions
  tags          = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "commands_dlq_visible" {
  alarm_name          = "${var.resource_plane_id}-commands-dlq-visible"
  alarm_description   = "Command messages reached the dead-letter queue; inspect and redrive only after diagnosis."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.commands_dlq.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "results_dlq_visible" {
  alarm_name          = "${var.resource_plane_id}-results-dlq-visible"
  alarm_description   = "Operation results reached the dead-letter queue; reconcile durable operation state before redriving."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.results_dlq.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "commands_oldest_message" {
  alarm_name          = "${var.resource_plane_id}-commands-oldest-message"
  alarm_description   = "A Resource Plane command has been waiting longer than the configured age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = ceil(var.health_report_age_alarm_seconds / 60)
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.commands.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "results_oldest_message" {
  alarm_name          = "${var.resource_plane_id}-results-oldest-message"
  alarm_description   = "An operation result has been waiting longer than the configured age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.results.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "commands_backlog" {
  alarm_name          = "${var.resource_plane_id}-commands-backlog"
  alarm_description   = "The Resource Plane command queue backlog exceeded its configured threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.queue_backlog_alarm_messages
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.commands.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "results_backlog" {
  alarm_name          = "${var.resource_plane_id}-results-backlog"
  alarm_description   = "The operation result queue backlog exceeded its configured threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.queue_backlog_alarm_messages
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.results.name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "workload_results_dlq_visible" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-results-dlq-visible"
  alarm_description   = "Workload Run results reached their dedicated dead-letter queue; inspect before redriving."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_results_dlq[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-results-dlq" })
}

resource "aws_cloudwatch_metric_alarm" "workload_results_oldest_message" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-results-oldest-message"
  alarm_description   = "A Workload Run result has been waiting longer than the configured age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_results[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-results" })
}

resource "aws_cloudwatch_metric_alarm" "workload_results_backlog" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-results-backlog"
  alarm_description   = "The dedicated Workload Run result queue backlog exceeded its configured threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.queue_backlog_alarm_messages
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_results[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-results" })
}

resource "aws_cloudwatch_metric_alarm" "workload_commands_dlq_visible" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-commands-dlq-visible"
  alarm_description   = "Workload dispatch messages reached the dead-letter queue; inspect the durable attempt journal before redriving."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_commands_dlq[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-commands-dlq" })
}

resource "aws_cloudwatch_metric_alarm" "workload_commands_oldest_message" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-commands-oldest-message"
  alarm_description   = "A Workload dispatch command has been waiting longer than the configured age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = ceil(var.health_report_age_alarm_seconds / 60)
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_commands[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-commands" })
}

resource "aws_cloudwatch_metric_alarm" "workload_commands_backlog" {
  count = var.enable_workload_result_queue ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-workload-commands-backlog"
  alarm_description   = "The Workload dispatch queue has reached the configured backlog threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.queue_backlog_alarm_messages
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.workload_commands[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "workload-commands" })
}

resource "aws_sqs_queue_policy" "commands" {
  queue_url = aws_sqs_queue.commands.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowControlPlaneDispatcherSend"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_dispatcher_role_arn }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.commands.arn
    }]
  })
}

resource "aws_sqs_queue_policy" "preview_runtime_commands" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_commands[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlaneDispatcher"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_dispatcher_role_arn }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.preview_runtime_commands[0].arn
    }]
  })
}

resource "aws_sqs_queue_policy" "preview_runtime_results" {
  count = var.enable_preview_runtime ? 1 : 0

  queue_url = aws_sqs_queue.preview_runtime_results[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlanePreviewResultConsumer"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_preview_result_consumer_role_arn }
      Action    = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
      Resource  = aws_sqs_queue.preview_runtime_results[0].arn
    }]
  })
}

resource "aws_sqs_queue_policy" "results" {
  queue_url = aws_sqs_queue.results.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowControlPlaneResultConsumer"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_result_consumer_role_arn }
      Action = [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes"
      ]
      Resource = aws_sqs_queue.results.arn
    }]
  })
}

resource "aws_sqs_queue_policy" "workload_results" {
  count = var.enable_workload_result_queue ? 1 : 0

  queue_url = aws_sqs_queue.workload_results[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlaneWorkloadResultConsumer"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_workload_result_consumer_role_arn }
      Action = [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes"
      ]
      Resource = aws_sqs_queue.workload_results[0].arn
    }]
  })
}

resource "aws_sqs_queue_policy" "workload_commands" {
  count = var.enable_workload_result_queue ? 1 : 0

  queue_url = aws_sqs_queue.workload_commands[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowExactControlPlaneDispatcherSend"
      Effect    = "Allow"
      Principal = { AWS = var.control_plane_dispatcher_role_arn }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.workload_commands[0].arn
    }]
  })
}

data "aws_iam_policy_document" "resource_controller_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "resource_controller" {
  name               = "${var.resource_plane_id}-resource-controller"
  assume_role_policy = data.aws_iam_policy_document.resource_controller_assume_role.json
  tags               = local.common_tags
}

resource "aws_cloudwatch_log_group" "resource_controller" {
  name              = "/aws/lambda/${var.resource_plane_id}-resource-controller"
  retention_in_days = var.log_retention_days
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "workload_controller" {
  count = var.enable_workload_result_queue ? 1 : 0

  name              = "/aws/lambda/${var.resource_plane_id}-workload-controller"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "workload-controller" })
}

data "aws_iam_policy_document" "workload_controller_assume_role" {
  count = var.enable_workload_result_queue ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-workload-controller"]
    }
  }
}

resource "aws_iam_role" "workload_controller" {
  count = var.enable_workload_result_queue ? 1 : 0

  name               = "${var.resource_plane_id}-workload-controller"
  assume_role_policy = data.aws_iam_policy_document.workload_controller_assume_role[0].json
  tags               = merge(local.common_tags, { "hako:component" = "workload-controller" })
}

resource "aws_iam_role_policy" "workload_controller" {
  count = var.enable_workload_result_queue ? 1 : 0

  name = "${var.resource_plane_id}-workload-controller-queues"
  role = aws_iam_role.workload_controller[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ConsumeOnlyDedicatedWorkloadCommands"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.workload_commands[0].arn
      },
      {
        Sid      = "PublishOnlyWorkloadRunResults"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = aws_sqs_queue.workload_results[0].arn
      },
      {
        Sid      = "WriteOnlyWorkloadControllerLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.workload_controller[0].arn}:*"
      }
      ], length(var.workload_controller_secret_arns) == 0 ? [] : [{
        Sid      = "ReadOnlyExplicitWorkloadControllerSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.workload_controller_secret_arns
        }], var.workload_controller_deployment == null ? [] : tolist([{
          Sid    = "CreateLambdaVpcNetworkInterfaces"
          Effect = "Allow"
          Action = ["ec2:CreateNetworkInterface"]
          Resource = concat(
            ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"],
            [for id in var.workload_controller_vpc_config.subnet_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${id}"],
            [for id in var.workload_controller_vpc_config.security_group_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:security-group/${id}"]
          )
          }, {
          Sid    = "ManageLambdaVpcNetworkInterfaces"
          Effect = "Allow"
          Action = ["ec2:AssignPrivateIpAddresses", "ec2:DeleteNetworkInterface", "ec2:UnassignPrivateIpAddresses"]
          Resource = [
            "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"
          ]
          }, {
          Sid      = "DescribeLambdaVpcNetworkInterfaces"
          Effect   = "Allow"
          Action   = ["ec2:DescribeNetworkInterfaces"]
          Resource = ["*"]
    }]))
  })
}

resource "aws_iam_role_policy" "workload_controller_runtime" {
  count = var.workload_controller_runtime == null ? 0 : 1

  name = "${var.resource_plane_id}-workload-controller-runtime"
  role = aws_iam_role.workload_controller[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "OperateOnlyConfiguredMicroVMImage"
        Effect   = "Allow"
        Action   = ["lambda:RunMicrovm", "lambda:GetMicrovm", "lambda:CreateMicrovmAuthToken", "lambda:TerminateMicrovm"]
        Resource = var.workload_controller_runtime.microvm_image_arn
      },
      {
        Sid      = "ConnectAndPassNetworkConnector"
        Effect   = "Allow"
        Action   = ["lambda:ConnectMicrovm", "lambda:PassNetworkConnector"]
        Resource = "*"
      },
      {
        Sid      = "JournalObjectReadWrite"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${var.workload_controller_runtime.journal_bucket_arn}/${var.workload_controller_runtime.journal_prefix}/*"
      },
      {
        Sid      = "ListOnlyJournalPrefixForRecovery"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = var.workload_controller_runtime.journal_bucket_arn
        Condition = {
          StringLike = {
            "s3:prefix" = ["${var.workload_controller_runtime.journal_prefix}/", "${var.workload_controller_runtime.journal_prefix}/*"]
          }
        }
      },
      {
        Sid      = "RecoveryLeaseObjectReadWrite"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = var.workload_controller_runtime.recovery_lease_object_arn
      },
      {
        Sid      = "SourceArchiveReadWrite"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${var.workload_controller_runtime.source_bucket_arn}/${var.workload_controller_runtime.source_prefix}/*"
      },
      {
        Sid      = "AttemptLogWriteAndExpiredObjectCleanup"
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:DeleteObject"]
        Resource = "${var.workload_controller_runtime.artifact_bucket_arn}/${var.workload_controller_runtime.artifact_prefix}/*"
      }
      ], var.workload_controller_runtime.source_kms_key_arn == "" ? [] : [{
        Sid      = "SourceArchiveKMSUse"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.workload_controller_runtime.source_kms_key_arn
        }], var.workload_controller_runtime.artifact_kms_key_arn == "" ? [] : [{
        Sid      = "AttemptLogKMSGenerateDataKey"
        Effect   = "Allow"
        Action   = ["kms:GenerateDataKey"]
        Resource = var.workload_controller_runtime.artifact_kms_key_arn
        }], length(var.workload_controller_runtime.secret_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyConfiguredSecretKMSKeys"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.workload_controller_runtime.secret_kms_key_arns
    }])
  })
}

# Artifact storage is a separate opt-in from the runtime IAM scope. This allows
# existing externally managed buckets to remain supported while new Resource
# Planes can provision a private, versioned bucket with bounded retention.
resource "aws_s3_bucket" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = split(":::", var.workload_controller_runtime.artifact_bucket_arn)[1]

  force_destroy = false

  tags = merge(local.common_tags, {
    "hako:component" = "workload-artifacts"
  })
}

resource "aws_s3_bucket_ownership_controls" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = var.workload_controller_runtime.artifact_kms_key_arn == "" ? "AES256" : "aws:kms"
      kms_master_key_id = var.workload_controller_runtime.artifact_kms_key_arn == "" ? null : var.workload_controller_runtime.artifact_kms_key_arn
    }
    bucket_key_enabled = var.workload_controller_runtime.artifact_kms_key_arn != ""
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id

  rule {
    id     = "hako-attempt-artifact-retention"
    status = "Enabled"

    filter {
      prefix = "${var.workload_controller_runtime.artifact_prefix}/"
    }

    expiration {
      days = var.workload_artifact_storage.retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = var.workload_artifact_storage.retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }

  rule {
    id     = "hako-expired-artifact-delete-markers"
    status = "Enabled"

    filter {
      prefix = "${var.workload_controller_runtime.artifact_prefix}/"
    }

    expiration {
      expired_object_delete_marker = true
    }
  }
}

resource "aws_s3_bucket_policy" "workload_artifacts" {
  count  = var.workload_artifact_storage == null ? 0 : 1
  bucket = aws_s3_bucket.workload_artifacts[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.workload_artifacts[0].arn,
          "${aws_s3_bucket.workload_artifacts[0].arn}/*",
        ]
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
      {
        Sid       = "AllowControllerAttemptArtifactAccess"
        Effect    = "Allow"
        Principal = { AWS = aws_iam_role.workload_controller[0].arn }
        Action    = ["s3:PutObject", "s3:DeleteObject"]
        Resource  = "${aws_s3_bucket.workload_artifacts[0].arn}/${var.workload_controller_runtime.artifact_prefix}/*"
      },
    ]
  })
}

# The production Workload Controller is opt-in and consumes only the dedicated
# Workload command queue. The same binary accepts the bounded recovery
# schedule envelope; no Workspace command queue or Control Plane DB access is
# added to this role.
data "aws_route_table" "workload_controller_private_subnet" {
  for_each = var.workload_controller_deployment == null ? toset([]) : toset(var.workload_controller_vpc_config.subnet_ids)

  subnet_id = each.value
}

resource "aws_lambda_function" "workload_controller" {
  count = var.workload_controller_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-workload-controller"
  role                           = aws_iam_role.workload_controller[0].arn
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  filename                       = var.workload_controller_deployment.zip_path
  source_code_hash               = filebase64sha256(var.workload_controller_deployment.zip_path)
  memory_size                    = var.workload_controller_deployment.lambda_memory_size
  timeout                        = var.workload_controller_deployment.lambda_timeout_seconds
  reserved_concurrent_executions = var.workload_controller_deployment.reserved_concurrent_executions

  environment {
    variables = {
      HAKO_WORKLOAD_LAMBDA_MODE                      = "combined"
      HAKO_RESOURCE_PLANE_ID                         = var.resource_plane_id
      HAKO_WORKLOAD_CONTROL_PLANE_ENDPOINT           = var.workload_controller_deployment.control_plane_endpoint
      HAKO_WORKLOAD_CONTROL_PLANE_HMAC_SECRET_ARN    = var.workload_controller_deployment.control_plane_hmac_secret_arn
      HAKO_WORKLOAD_ARTIFACT_CLEANUP_HMAC_SECRET_ARN = var.workload_controller_deployment.artifact_cleanup_hmac_secret_arn
      HAKO_WORKLOAD_CHECKOUT_BROKER_URL              = var.workload_controller_deployment.broker_url
      HAKO_WORKLOAD_BROKER_CLIENT_CERT_SECRET_ARN    = var.workload_controller_deployment.broker_client_cert_secret_arn
      HAKO_WORKLOAD_BROKER_CLIENT_KEY_SECRET_ARN     = var.workload_controller_deployment.broker_client_key_secret_arn
      HAKO_WORKLOAD_BROKER_CA_SECRET_ARN             = var.workload_controller_deployment.broker_ca_secret_arn
      HAKO_WORKLOAD_JOURNAL_BUCKET                   = split(":::", var.workload_controller_runtime.journal_bucket_arn)[1]
      HAKO_WORKLOAD_JOURNAL_PREFIX                   = var.workload_controller_runtime.journal_prefix
      HAKO_WORKLOAD_RECOVERY_LEASE_KEY               = trimprefix(var.workload_controller_runtime.recovery_lease_object_arn, "${var.workload_controller_runtime.journal_bucket_arn}/")
      HAKO_WORKLOAD_SOURCE_BUCKET                    = split(":::", var.workload_controller_runtime.source_bucket_arn)[1]
      HAKO_WORKLOAD_SOURCE_PREFIX                    = var.workload_controller_runtime.source_prefix
      HAKO_WORKLOAD_SOURCE_KMS_KEY_ID                = var.workload_controller_runtime.source_kms_key_arn
      HAKO_WORKLOAD_ARTIFACT_BUCKET                  = split(":::", var.workload_controller_runtime.artifact_bucket_arn)[1]
      HAKO_WORKLOAD_ARTIFACT_PREFIX                  = var.workload_controller_runtime.artifact_prefix
      HAKO_WORKLOAD_ARTIFACT_KMS_KEY_ID              = var.workload_controller_runtime.artifact_kms_key_arn
      HAKO_WORKLOAD_RESULT_QUEUE_URL                 = aws_sqs_queue.workload_results[0].url
      HAKO_WORKLOAD_TICKET_KEY_ID                    = var.workload_controller_deployment.ticket_key_id
      HAKO_WORKLOAD_TICKET_PRIVATE_KEY_SECRET_ARN    = var.workload_controller_deployment.ticket_private_key_secret_arn
      HAKO_WORKLOAD_RUNTIME_PROFILES_JSON            = var.workload_controller_deployment.runtime_profiles_json
      HAKO_WORKLOAD_RECOVERY_PAGE_SIZE               = tostring(var.workload_controller_deployment.recovery_page_size)
      HAKO_WORKLOAD_RECOVERY_MAX_PAGES               = tostring(var.workload_controller_deployment.recovery_max_pages)
      HAKO_WORKLOAD_RECOVERY_LEASE_TTL               = var.workload_controller_deployment.recovery_lease_ttl
      HAKO_WORKLOAD_ARTIFACT_CLEANUP_BATCH_SIZE      = tostring(var.workload_controller_deployment.artifact_cleanup_batch_size)
    }
  }

  dynamic "vpc_config" {
    for_each = var.workload_controller_deployment == null ? [] : [var.workload_controller_vpc_config]
    content {
      subnet_ids         = vpc_config.value.subnet_ids
      security_group_ids = vpc_config.value.security_group_ids
    }
  }

  lifecycle {
    precondition {
      condition = alltrue([for route_table in values(data.aws_route_table.workload_controller_private_subnet) :
        anytrue([for route in route_table.routes :
          try(route.cidr_block == "0.0.0.0/0" && route.nat_gateway_id != "", false)
        ])
      ])
      error_message = "Every Workload Controller subnet must have a 0.0.0.0/0 route targeting a NAT Gateway. Hako does not create or modify NAT, routes, VPC endpoints, or security groups."
    }
  }

  depends_on = [aws_iam_role_policy.workload_controller, aws_iam_role_policy.workload_controller_runtime]
  tags       = merge(local.common_tags, { "hako:component" = "workload-controller" })
}

resource "aws_lambda_event_source_mapping" "workload_controller" {
  count = var.workload_controller_deployment == null ? 0 : 1

  event_source_arn        = aws_sqs_queue.workload_commands[0].arn
  function_name           = aws_lambda_function.workload_controller[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true
}

resource "aws_cloudwatch_event_rule" "workload_controller_recovery" {
  count = var.workload_controller_deployment == null ? 0 : 1

  name                = "${var.resource_plane_id}-workload-recovery"
  description         = "Bounded lease-coordinated Lambda MicroVM attempt recovery."
  schedule_expression = var.workload_controller_deployment.recovery_schedule_expression
  tags                = merge(local.common_tags, { "hako:component" = "workload-controller-recovery" })
}

resource "aws_cloudwatch_event_target" "workload_controller_recovery" {
  count = var.workload_controller_deployment == null ? 0 : 1

  rule      = aws_cloudwatch_event_rule.workload_controller_recovery[0].name
  target_id = "workload-controller"
  arn       = aws_lambda_function.workload_controller[0].arn

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "workload_controller_recovery" {
  count = var.workload_controller_deployment == null ? 0 : 1

  statement_id  = "AllowWorkloadRecoverySchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.workload_controller[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.workload_controller_recovery[0].arn
}

resource "aws_cloudwatch_event_rule" "workload_artifact_cleanup" {
  count = var.workload_controller_deployment == null ? 0 : 1

  name                = "${var.resource_plane_id}-workload-artifact-cleanup"
  description         = "Bounded expiry cleanup for Workload Artifacts in the configured prefix."
  schedule_expression = var.workload_controller_deployment.artifact_cleanup_schedule_expression
  tags                = merge(local.common_tags, { "hako:component" = "workload-artifact-cleanup" })
}

resource "aws_cloudwatch_event_target" "workload_artifact_cleanup" {
  count = var.workload_controller_deployment == null ? 0 : 1

  rule      = aws_cloudwatch_event_rule.workload_artifact_cleanup[0].name
  target_id = "workload-controller-artifact-cleanup"
  arn       = aws_lambda_function.workload_controller[0].arn
  input     = jsonencode({ source = "aws.events", hako_task = "artifact-cleanup" })

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "workload_artifact_cleanup" {
  count = var.workload_controller_deployment == null ? 0 : 1

  statement_id  = "AllowWorkloadArtifactCleanupSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.workload_controller[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.workload_artifact_cleanup[0].arn
}

resource "aws_cloudwatch_metric_alarm" "workload_controller_errors" {
  count = var.workload_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workload-controller-errors"
  alarm_description   = "Workload command or scheduled recovery Lambda invocation errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.workload_controller[0].function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  tags                = merge(local.common_tags, { "hako:component" = "workload-controller" })
}

resource "aws_cloudwatch_metric_alarm" "workload_recovery_failed_invocations" {
  count = var.workload_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workload-recovery-failed-invocations"
  alarm_description   = "EventBridge could not invoke the scheduled Workload recovery Lambda."
  namespace           = "AWS/Events"
  metric_name         = "FailedInvocations"
  dimensions          = { RuleName = aws_cloudwatch_event_rule.workload_controller_recovery[0].name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  tags                = merge(local.common_tags, { "hako:component" = "workload-controller-recovery" })
}

resource "aws_cloudwatch_metric_alarm" "workload_artifact_cleanup_failed_invocations" {
  count = var.workload_controller_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workload-artifact-cleanup-failed-invocations"
  alarm_description   = "EventBridge could not invoke the scheduled Workload Artifact cleanup Lambda."
  namespace           = "AWS/Events"
  metric_name         = "FailedInvocations"
  dimensions          = { RuleName = aws_cloudwatch_event_rule.workload_artifact_cleanup[0].name }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  tags                = merge(local.common_tags, { "hako:component" = "workload-artifact-cleanup" })
}

resource "aws_cloudwatch_log_group" "fake_resource_controller" {
  count = var.enable_fake_resource_controller ? 1 : 0

  name              = "/aws/lambda/${var.resource_plane_id}-fake-controller"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:runtime" = "fake-only" })
}

resource "aws_cloudwatch_log_group" "health_reporter" {
  count = var.enable_health_report_alarms || var.enable_health_reporter ? 1 : 0

  name              = "/aws/lambda/${var.resource_plane_id}-health-reporter"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

data "aws_iam_policy_document" "health_reporter_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-health-reporter"]
    }
  }
}

resource "aws_iam_role" "health_reporter" {
  count = var.enable_health_reporter ? 1 : 0

  name               = "${var.resource_plane_id}-health-reporter"
  assume_role_policy = data.aws_iam_policy_document.health_reporter_assume_role.json
  tags               = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_iam_role_policy" "health_reporter" {
  count = var.enable_health_reporter ? 1 : 0

  name = "${var.resource_plane_id}-health-reporter-minimum"
  role = aws_iam_role.health_reporter[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadOnlyPlaneHealthHMACKey"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.health_report_secret_arn
      },
      {
        Sid      = "ProbeOnlyPlaneQueues"
        Effect   = "Allow"
        Action   = ["sqs:GetQueueAttributes"]
        Resource = [aws_sqs_queue.commands.arn, aws_sqs_queue.results.arn]
      },
      {
        Sid      = "ReadOnlyConfiguredMicroVMImageVersion"
        Effect   = "Allow"
        Action   = ["lambda:GetMicrovmImageVersion"]
        Resource = var.health_report_microvm_image_arn
      },
      {
        Sid      = "ReadOnlyConfiguredVPCEgressConnector"
        Effect   = "Allow"
        Action   = ["lambda:GetNetworkConnector"]
        Resource = var.health_report_vpc_egress_connector_arn
      },
      {
        Sid      = "ReadOnlyConfiguredWorkloadController"
        Effect   = "Allow"
        Action   = ["lambda:GetFunctionConfiguration"]
        Resource = var.health_report_controller_function_arn
      },
      {
        Sid      = "ReadOnlyConfiguredWorkspaceEFS"
        Effect   = "Allow"
        Action   = ["elasticfilesystem:DescribeFileSystems"]
        Resource = var.health_report_workspace_efs_arn
      },
      {
        Sid      = "WriteOnlyHealthReporterLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.health_reporter[0].arn}:*"
      }
    ]
  })
}

resource "aws_lambda_function" "health_reporter" {
  count = var.enable_health_reporter ? 1 : 0

  function_name    = "${var.resource_plane_id}-health-reporter"
  role             = aws_iam_role.health_reporter[0].arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = var.health_reporter_lambda_zip_path
  source_code_hash = filebase64sha256(var.health_reporter_lambda_zip_path)
  memory_size      = 256
  timeout          = 20

  environment {
    variables = {
      HAKO_RESOURCE_PLANE_ID        = var.resource_plane_id
      HAKO_CONTROL_PLANE_ENDPOINT   = var.health_report_control_plane_endpoint
      HAKO_HEALTH_REPORT_SECRET_ARN = var.health_report_secret_arn
      HAKO_COMMAND_QUEUE_URL        = aws_sqs_queue.commands.url
      HAKO_RESULT_QUEUE_URL         = aws_sqs_queue.results.url
      HAKO_MICROVM_IMAGE_ARN        = var.health_report_microvm_image_arn
      HAKO_MICROVM_IMAGE_VERSION    = var.health_report_microvm_image_version
      HAKO_VPC_EGRESS_CONNECTOR_ARN = var.health_report_vpc_egress_connector_arn
      HAKO_CONTROLLER_FUNCTION_ARN  = var.health_report_controller_function_arn
      HAKO_WORKSPACE_EFS_ARN        = var.health_report_workspace_efs_arn
    }
  }

  depends_on = [aws_cloudwatch_log_group.health_reporter, aws_iam_role_policy.health_reporter]
  tags       = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_event_rule" "health_reporter_schedule" {
  count = var.enable_health_reporter ? 1 : 0

  name                = "${var.resource_plane_id}-health-report"
  description         = "Run the Resource Plane capability health reporter once per minute."
  schedule_expression = "rate(1 minute)"
  state               = "ENABLED"
  tags                = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_event_target" "health_reporter" {
  count = var.enable_health_reporter ? 1 : 0

  rule      = aws_cloudwatch_event_rule.health_reporter_schedule[0].name
  target_id = "${var.resource_plane_id}-health-reporter"
  arn       = aws_lambda_function.health_reporter[0].arn
}

resource "aws_lambda_permission" "health_reporter_schedule" {
  count = var.enable_health_reporter ? 1 : 0

  statement_id  = "AllowScheduledHealthReporter"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.health_reporter[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.health_reporter_schedule[0].arn
}

locals {
  health_report_metric_dimensions = { ResourcePlaneId = var.resource_plane_id }
}

resource "aws_cloudwatch_metric_alarm" "health_report_stale" {
  count = var.enable_health_report_alarms ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-health-report-stale"
  alarm_description   = "No fresh Resource Plane health report was observed within the configured freshness window."
  namespace           = "Hako/ResourcePlaneHealth"
  metric_name         = "HealthReportHeartbeat"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = local.health_report_metric_dimensions
  tags                = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_metric_alarm" "health_report_unhealthy" {
  count = var.enable_health_report_alarms ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-health-report-unhealthy"
  alarm_description   = "Repeated Resource Plane probe runs report unhealthy; an OK transition is the recovery notification."
  namespace           = "Hako/ResourcePlaneHealth"
  metric_name         = "HealthUnhealthy"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = var.health_report_unhealthy_runs_threshold
  datapoints_to_alarm = var.health_report_unhealthy_runs_threshold
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = local.health_report_metric_dimensions
  tags                = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_metric_alarm" "health_report_probe_failures" {
  count = var.enable_health_report_alarms ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-health-report-probe-failures"
  alarm_description   = "One or more Resource Plane capability probes failed; partial failures are reported as degraded."
  namespace           = "Hako/ResourcePlaneHealth"
  metric_name         = "ProbeFailures"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = local.health_report_metric_dimensions
  tags                = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_metric_alarm" "health_reporter_errors" {
  count = var.enable_health_report_alarms ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-health-reporter-errors"
  alarm_description   = "Health report attempts failed to reach or update the Control Plane."
  namespace           = "Hako/ResourcePlaneHealth"
  metric_name         = "ReporterFailures"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = local.health_report_metric_dimensions
  tags                = merge(local.common_tags, { "hako:component" = "health-reporter" })
}

resource "aws_cloudwatch_metric_alarm" "fake_controller_errors" {
  count = var.enable_fake_resource_controller ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-fake-controller-errors"
  alarm_description   = "The development-only Fake Resource Controller Lambda reported invocation errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { FunctionName = aws_lambda_function.fake_resource_controller[0].function_name }
  tags                = merge(local.common_tags, { "hako:runtime" = "fake-only" })
}

data "aws_iam_policy_document" "resource_controller" {
  statement {
    sid       = "ConsumeCommands"
    effect    = "Allow"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
    resources = [aws_sqs_queue.commands.arn]
  }

  statement {
    sid       = "PublishResults"
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.results.arn]
  }

  dynamic "statement" {
    for_each = var.enable_preview_runtime ? [1] : []
    content {
      sid       = "ConsumePreviewRuntimeCommands"
      effect    = "Allow"
      actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
      resources = [aws_sqs_queue.preview_runtime_commands[0].arn]
    }
  }

  dynamic "statement" {
    for_each = var.enable_preview_runtime ? [1] : []
    content {
      sid       = "PublishPreviewRuntimeResults"
      effect    = "Allow"
      actions   = ["sqs:SendMessage"]
      resources = [aws_sqs_queue.preview_runtime_results[0].arn]
    }
  }

  statement {
    sid     = "WriteControllerLogs"
    effect  = "Allow"
    actions = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = concat(
      ["${aws_cloudwatch_log_group.resource_controller.arn}:*"],
      var.enable_fake_resource_controller ? ["${aws_cloudwatch_log_group.fake_resource_controller[0].arn}:*"] : []
    )
  }
}

resource "aws_iam_role_policy" "resource_controller" {
  name   = "${var.resource_plane_id}-resource-controller-runtime"
  role   = aws_iam_role.resource_controller.id
  policy = data.aws_iam_policy_document.resource_controller.json
}

# Workspace Access Point and execution-role lifecycle is separately opt-in and
# fenced to one configured EFS filesystem plus Hako's dedicated role path.
resource "aws_iam_role_policy" "resource_controller_workspace_storage" {
  count = var.workspace_storage_runtime == null ? 0 : 1

  name = "${var.resource_plane_id}-resource-controller-workspace-storage"
  role = aws_iam_role.resource_controller.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "CreateWorkspaceAccessPointsOnExactFilesystem"
        Effect   = "Allow"
        Action   = ["elasticfilesystem:CreateAccessPoint"]
        Resource = var.workspace_storage_runtime.file_system_arn
        Condition = {
          StringEquals = {
            "aws:RequestTag/hako:managed-by"        = "hako"
            "aws:RequestTag/hako:resource-plane-id" = var.resource_plane_id
          }
          StringLike = {
            "aws:RequestTag/hako:tenant-id"    = "?*"
            "aws:RequestTag/hako:workspace-id" = "?*"
          }
          "ForAllValues:StringEquals" = {
            "aws:TagKeys" = ["hako:managed-by", "hako:tenant-id", "hako:workspace-id", "hako:resource-plane-id"]
          }
        }
      },
      {
        Sid      = "TagOnlyNewWorkspaceAccessPoints"
        Effect   = "Allow"
        Action   = ["elasticfilesystem:TagResource"]
        Resource = "arn:${data.aws_partition.current.partition}:elasticfilesystem:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:access-point/fsap-*"
        Condition = {
          StringEquals = {
            "elasticfilesystem:CreateAction"        = "CreateAccessPoint"
            "aws:RequestTag/hako:managed-by"        = "hako"
            "aws:RequestTag/hako:resource-plane-id" = var.resource_plane_id
          }
          StringLike = {
            "aws:RequestTag/hako:tenant-id"    = "?*"
            "aws:RequestTag/hako:workspace-id" = "?*"
          }
          "ForAllValues:StringEquals" = {
            "aws:TagKeys" = ["hako:managed-by", "hako:tenant-id", "hako:workspace-id", "hako:resource-plane-id"]
          }
        }
      },
      {
        Sid    = "DescribeOnlyWorkspaceStorageResources"
        Effect = "Allow"
        Action = ["elasticfilesystem:DescribeAccessPoints"]
        Resource = [
          var.workspace_storage_runtime.file_system_arn,
          "arn:${data.aws_partition.current.partition}:elasticfilesystem:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:access-point/fsap-*"
        ]
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by"        = "hako"
            "aws:ResourceTag/hako:resource-plane-id" = var.resource_plane_id
          }
        }
      },
      {
        Sid      = "DeleteOnlyOwnedWorkspaceAccessPoints"
        Effect   = "Allow"
        Action   = ["elasticfilesystem:DeleteAccessPoint"]
        Resource = "arn:${data.aws_partition.current.partition}:elasticfilesystem:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:access-point/fsap-*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by"        = "hako"
            "aws:ResourceTag/hako:resource-plane-id" = var.resource_plane_id
          }
        }
      },
      {
        Sid      = "CreateOnlyTaggedWorkspaceExecutionRoles"
        Effect   = "Allow"
        Action   = ["iam:CreateRole"]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/*"
        Condition = {
          StringEquals = {
            "aws:RequestTag/hako:managed-by"        = "hako"
            "aws:RequestTag/hako:resource-plane-id" = var.resource_plane_id
          }
          StringLike = {
            "aws:RequestTag/hako:tenant-id"    = "?*"
            "aws:RequestTag/hako:workspace-id" = "?*"
          }
          "ForAllValues:StringEquals" = {
            "aws:TagKeys" = ["hako:managed-by", "hako:tenant-id", "hako:workspace-id", "hako:resource-plane-id"]
          }
        }
      },
      {
        Sid      = "TagOnlyWorkspaceExecutionRolesAtCreation"
        Effect   = "Allow"
        Action   = ["iam:TagRole"]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/*"
        Condition = {
          StringEquals = {
            "aws:RequestTag/hako:managed-by"        = "hako"
            "aws:RequestTag/hako:resource-plane-id" = var.resource_plane_id
          }
          StringLike = {
            "aws:RequestTag/hako:tenant-id"    = "?*"
            "aws:RequestTag/hako:workspace-id" = "?*"
          }
          "ForAllValues:StringEquals" = {
            "aws:TagKeys" = ["hako:managed-by", "hako:tenant-id", "hako:workspace-id", "hako:resource-plane-id"]
          }
        }
      },
      {
        Sid    = "ManageOnlyTaggedWorkspaceExecutionRoles"
        Effect = "Allow"
        Action = [
          "iam:DeleteRole", "iam:GetRole", "iam:PutRolePolicy", "iam:GetRolePolicy",
          "iam:ListRolePolicies", "iam:ListAttachedRolePolicies", "iam:DeleteRolePolicy"
        ]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/*"
        Condition = {
          StringEquals = {
            "aws:ResourceTag/hako:managed-by"        = "hako"
            "aws:ResourceTag/hako:resource-plane-id" = var.resource_plane_id
          }
        }
      },
      {
        Sid      = "PassOnlyWorkspaceExecutionRolesToLambda"
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/*"
        Condition = {
          StringEquals = { "iam:PassedToService" = "lambda.amazonaws.com" }
        }
      }
    ]
  })

  lifecycle {
    precondition {
      condition = (
        split(":", var.workspace_storage_runtime.file_system_arn)[3] == data.aws_region.current.name &&
        split(":", var.workspace_storage_runtime.file_system_arn)[4] == data.aws_caller_identity.current.account_id &&
        endswith(var.workspace_storage_runtime.file_system_arn, "/${var.workspace_storage_runtime.file_system_id}")
      )
      error_message = "Workspace runtime IAM may manage only an EFS filesystem in this Resource Plane account and Region."
    }
  }
}

# The production Workspace Resource Controller is explicit and default-off.
# RunMicrovm lifecycle permissions are restricted to one fixed image; the two
# permission-only network actions require Resource="*" per AWS authorization
# metadata, while the launch profile fixes the connector ARNs. S3 journal access
# is limited to exact objects below one bucket prefix. Dynamic Workspace EFS/IAM
# permissions are attached separately above and are required by this runtime.
resource "aws_iam_role_policy" "resource_controller_workspace_runtime" {
  count = var.workspace_runtime_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-resource-controller-workspace-runtime"
  role = aws_iam_role.resource_controller.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid    = "OperateOnlyConfiguredWorkspaceMicroVMImage"
        Effect = "Allow"
        Action = [
          "lambda:RunMicrovm", "lambda:GetMicrovm", "lambda:SuspendMicrovm",
          "lambda:ResumeMicrovm", "lambda:TerminateMicrovm"
        ]
        Resource = var.workspace_runtime_deployment.microvm_image_arn
      },
      {
        Sid      = "ConnectAndPassConfiguredMicroVMNetworks"
        Effect   = "Allow"
        Action   = ["lambda:ConnectMicrovm", "lambda:PassNetworkConnector"]
        Resource = "*"
      },
      {
        Sid      = "ReadWriteOnlyWorkspaceRuntimeJournalPrefix"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${var.workspace_runtime_deployment.journal_bucket_arn}/${var.workspace_runtime_deployment.journal_prefix}/*"
      },
      ], var.workspace_runtime_deployment.journal_kms_key_arn == "" ? [] : [{
        Sid      = "UseOnlyConfiguredWorkspaceJournalKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.workspace_runtime_deployment.journal_kms_key_arn
    }])
  })

  lifecycle {
    precondition {
      condition = (
        split(":", var.workspace_runtime_deployment.microvm_image_arn)[3] == data.aws_region.current.name &&
        split(":", var.workspace_runtime_deployment.microvm_image_arn)[4] == data.aws_caller_identity.current.account_id &&
        var.workspace_storage_runtime != null
      )
      error_message = "Workspace runtime IAM must use an image in this Resource Plane account and Region and have dynamic EFS role management enabled."
    }
  }
}

# Preview source access is attached only when the dedicated Preview queue is
# enabled. Checkout tokens stay in memory; archive access is limited to one
# generation/claim prefix below the configured S3 source namespace.
resource "aws_iam_role_policy" "resource_controller_preview_source" {
  count = var.enable_preview_runtime ? 1 : 0

  name = "${var.resource_plane_id}-resource-controller-preview-source"
  role = aws_iam_role.resource_controller.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ReadOnlyConfiguredPreviewMicroVM"
        Effect   = "Allow"
        Action   = ["lambda:GetMicrovm", "lambda:CreateMicrovmAuthToken"]
        Resource = var.workspace_runtime_deployment.microvm_image_arn
      },
      {
        Sid      = "StageAndReadOnlyPreviewSourcePrefix"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${var.preview_runtime_source.source_bucket_arn}/${var.preview_runtime_source.source_prefix}/*"
      },
      {
        Sid    = "ReadOnlyPreviewRuntimeSecrets"
        Effect = "Allow"
        Action = ["secretsmanager:GetSecretValue"]
        Resource = [
          var.preview_runtime_source.broker_client_cert_arn,
          var.preview_runtime_source.broker_client_key_arn,
          var.preview_runtime_source.broker_ca_arn,
          var.preview_runtime_source.ticket_private_key_arn,
        ]
      },
      ], var.preview_runtime_source.source_kms_key_arn == "" ? [] : [{
        Sid      = "UseOnlyPreviewSourceArchiveKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.preview_runtime_source.source_kms_key_arn
    }])
  })
}

# The development-only Fake controller remains separate from the opt-in
# Lambda MicroVM controller below; both cannot consume the shared queue.
resource "aws_lambda_function" "fake_resource_controller" {
  count = var.enable_fake_resource_controller ? 1 : 0

  function_name    = "${var.resource_plane_id}-fake-controller"
  role             = aws_iam_role.resource_controller.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  filename         = var.fake_controller_lambda_zip_path
  source_code_hash = filebase64sha256(var.fake_controller_lambda_zip_path)
  memory_size      = var.fake_controller_memory_size
  timeout          = var.fake_controller_timeout_seconds

  environment {
    variables = {
      HAKO_RESOURCE_PLANE_ID          = var.resource_plane_id
      HAKO_OPERATION_RESULT_QUEUE_URL = aws_sqs_queue.results.url
      HAKO_RUNTIME_IMPLEMENTATION     = "fake"
      HAKO_OPERATION_TIMEOUT          = "${var.fake_controller_execution_timeout_seconds}s"
    }
  }

  depends_on = [aws_iam_role_policy.resource_controller]

  lifecycle {
    precondition {
      condition     = var.fake_controller_execution_timeout_seconds < var.fake_controller_timeout_seconds
      error_message = "fake controller execution timeout must be shorter than the Lambda timeout."
    }
  }

  tags = merge(local.common_tags, { "hako:runtime" = "fake-only" })
}

resource "aws_lambda_event_source_mapping" "fake_resource_controller" {
  count = var.enable_fake_resource_controller ? 1 : 0

  event_source_arn        = aws_sqs_queue.commands.arn
  function_name           = aws_lambda_function.fake_resource_controller[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true

  lifecycle {
    precondition {
      condition     = var.command_visibility_timeout_seconds >= var.fake_controller_timeout_seconds * 6
      error_message = "command queue visibility timeout must be at least six times the fake controller Lambda timeout."
    }
  }
}

resource "aws_lambda_function" "workspace_resource_controller" {
  count = var.workspace_runtime_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-resource-controller"
  role                           = aws_iam_role.resource_controller.arn
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  filename                       = var.workspace_runtime_deployment.zip_path
  source_code_hash               = filebase64sha256(var.workspace_runtime_deployment.zip_path)
  memory_size                    = var.workspace_runtime_deployment.lambda_memory_size
  timeout                        = var.workspace_runtime_deployment.lambda_timeout_seconds
  reserved_concurrent_executions = var.workspace_runtime_deployment.reserved_concurrent_executions

  environment {
    variables = {
      HAKO_RESOURCE_PLANE_ID                     = var.resource_plane_id
      HAKO_OPERATION_RESULT_QUEUE_URL            = aws_sqs_queue.results.url
      HAKO_RUNTIME_IMPLEMENTATION                = "lambda-microvm"
      HAKO_OPERATION_TIMEOUT                     = "${var.workspace_runtime_deployment.operation_timeout_seconds}s"
      HAKO_PREVIEW_RUNTIME_COMMAND_QUEUE_ARN     = var.enable_preview_runtime ? aws_sqs_queue.preview_runtime_commands[0].arn : ""
      HAKO_PREVIEW_RUNTIME_RESULT_QUEUE_URL      = var.enable_preview_runtime ? aws_sqs_queue.preview_runtime_results[0].url : ""
      HAKO_PREVIEW_CHECKOUT_BROKER_URL           = var.enable_preview_runtime ? var.preview_runtime_source.checkout_broker_url : ""
      HAKO_PREVIEW_BROKER_CLIENT_CERT_SECRET_ARN = var.enable_preview_runtime ? var.preview_runtime_source.broker_client_cert_arn : ""
      HAKO_PREVIEW_BROKER_CLIENT_KEY_SECRET_ARN  = var.enable_preview_runtime ? var.preview_runtime_source.broker_client_key_arn : ""
      HAKO_PREVIEW_BROKER_CA_SECRET_ARN          = var.enable_preview_runtime ? var.preview_runtime_source.broker_ca_arn : ""
      HAKO_PREVIEW_SOURCE_BUCKET                 = var.enable_preview_runtime ? var.preview_runtime_source.source_bucket_name : ""
      HAKO_PREVIEW_SOURCE_PREFIX                 = var.enable_preview_runtime ? var.preview_runtime_source.source_prefix : ""
      HAKO_PREVIEW_SOURCE_KMS_KEY_ID             = var.enable_preview_runtime ? var.preview_runtime_source.source_kms_key_arn : ""
      HAKO_PREVIEW_GATEWAY_ORIGIN                = var.enable_preview_runtime ? var.preview_runtime_source.gateway_origin : ""
      HAKO_PREVIEW_TICKET_KEY_ID                 = var.enable_preview_runtime ? var.preview_runtime_source.ticket_key_id : ""
      HAKO_PREVIEW_TICKET_PRIVATE_KEY_SECRET_ARN = var.enable_preview_runtime ? var.preview_runtime_source.ticket_private_key_arn : ""
      HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET      = var.workspace_runtime_deployment.journal_bucket_name
      HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX      = var.workspace_runtime_deployment.journal_prefix
      HAKO_WORKSPACE_EFS_FILE_SYSTEM_ID          = var.workspace_storage_runtime.file_system_id
      HAKO_WORKSPACE_RUNTIME_POLL_INTERVAL       = "500ms"
      HAKO_WORKSPACE_RUNTIME_POLL_TIMEOUT        = "30s"
      HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON = jsonencode({
        runtime_class               = var.workspace_runtime_deployment.runtime_class
        image_identifier            = var.workspace_runtime_deployment.microvm_image_arn
        image_version               = var.workspace_runtime_deployment.microvm_image_version
        execution_role_arn          = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/hako/workspaces/profile-placeholder"
        egress_network_connectors   = var.workspace_runtime_deployment.egress_network_connectors
        ingress_network_connectors  = var.workspace_runtime_deployment.ingress_network_connectors
        maximum_duration_in_seconds = var.workspace_runtime_deployment.maximum_duration_seconds
        auto_resume_enabled         = var.workspace_runtime_deployment.auto_resume_enabled
        max_idle_duration_seconds   = var.workspace_runtime_deployment.max_idle_duration_seconds
        suspended_duration_seconds  = var.workspace_runtime_deployment.suspended_duration_seconds
      })
    }
  }

  depends_on = [
    aws_iam_role_policy.resource_controller,
    aws_iam_role_policy.resource_controller_workspace_storage,
    aws_iam_role_policy.resource_controller_workspace_runtime,
    aws_iam_role_policy.resource_controller_preview_source
  ]

  lifecycle {
    precondition {
      condition     = var.workspace_storage_runtime != null && !var.enable_fake_resource_controller
      error_message = "The Workspace Lambda MicroVM controller requires dynamic Workspace EFS/IAM configuration and cannot share the command queue with the Fake controller."
    }
    precondition {
      condition     = var.command_visibility_timeout_seconds >= var.workspace_runtime_deployment.lambda_timeout_seconds * 6
      error_message = "Command queue visibility timeout must be at least six times the Workspace Resource Controller Lambda timeout."
    }
  }

  tags = merge(local.common_tags, { "hako:runtime" = "lambda-microvm" })
}

resource "aws_lambda_event_source_mapping" "workspace_resource_controller" {
  count = var.workspace_runtime_deployment == null ? 0 : 1

  event_source_arn        = aws_sqs_queue.commands.arn
  function_name           = aws_lambda_function.workspace_resource_controller[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true
}

resource "aws_lambda_event_source_mapping" "preview_runtime_controller" {
  count = var.enable_preview_runtime ? 1 : 0

  event_source_arn        = aws_sqs_queue.preview_runtime_commands[0].arn
  function_name           = aws_lambda_function.workspace_resource_controller[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true

  depends_on = [aws_iam_role_policy.resource_controller]
}

resource "aws_cloudwatch_metric_alarm" "preview_runtime_commands_dlq_visible" {
  count = var.enable_preview_runtime ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-preview-runtime-commands-dlq-visible"
  alarm_description   = "Preview runtime commands reached the dead-letter queue; inspect the generation claim and runtime journal before redriving."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.preview_runtime_commands_dlq[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-commands-dlq" })
}

resource "aws_cloudwatch_metric_alarm" "preview_runtime_commands_oldest_message" {
  count = var.enable_preview_runtime ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-preview-runtime-commands-oldest-message"
  alarm_description   = "A Preview runtime command has exceeded the Resource Plane processing age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.preview_runtime_commands[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-commands" })
}

resource "aws_cloudwatch_metric_alarm" "preview_runtime_results_dlq_visible" {
  count = var.enable_preview_runtime ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-preview-runtime-results-dlq-visible"
  alarm_description   = "Preview runtime results reached the dead-letter queue; reconcile the generation result before redriving."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.preview_runtime_results_dlq[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-results-dlq" })
}

resource "aws_cloudwatch_metric_alarm" "preview_runtime_results_oldest_message" {
  count = var.enable_preview_runtime ? 1 : 0

  alarm_name          = "${var.resource_plane_id}-preview-runtime-results-oldest-message"
  alarm_description   = "A Preview runtime result has exceeded the Control Plane acknowledgement age threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.oldest_message_age_alarm_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.preview_runtime_results[0].name }
  tags                = merge(local.common_tags, { "hako:queue-role" = "preview-runtime-results" })
}

resource "aws_cloudwatch_metric_alarm" "workspace_resource_controller_errors" {
  count = var.workspace_runtime_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-workspace-resource-controller-errors"
  alarm_description   = "The Lambda MicroVM Workspace Resource Controller reported invocation errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.alarm_actions
  ok_actions          = var.alarm_actions
  dimensions          = { FunctionName = aws_lambda_function.workspace_resource_controller[0].function_name }
  tags                = merge(local.common_tags, { "hako:runtime" = "lambda-microvm" })
}
