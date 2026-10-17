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
        Sid      = "AttemptLogWriteOnly"
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
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

# The production Workload Controller is opt-in and consumes only the dedicated
# Workload command queue. The same binary accepts the bounded recovery
# schedule envelope; no Workspace command queue or Control Plane DB access is
# added to this role.
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
      HAKO_WORKLOAD_LAMBDA_MODE                   = "combined"
      HAKO_RESOURCE_PLANE_ID                      = var.resource_plane_id
      HAKO_WORKLOAD_CONTROL_PLANE_ENDPOINT        = var.workload_controller_deployment.control_plane_endpoint
      HAKO_WORKLOAD_CONTROL_PLANE_HMAC_SECRET_ARN = var.workload_controller_deployment.control_plane_hmac_secret_arn
      HAKO_WORKLOAD_CHECKOUT_BROKER_URL           = var.workload_controller_deployment.broker_url
      HAKO_WORKLOAD_BROKER_CLIENT_CERT_SECRET_ARN = var.workload_controller_deployment.broker_client_cert_secret_arn
      HAKO_WORKLOAD_BROKER_CLIENT_KEY_SECRET_ARN  = var.workload_controller_deployment.broker_client_key_secret_arn
      HAKO_WORKLOAD_BROKER_CA_SECRET_ARN          = var.workload_controller_deployment.broker_ca_secret_arn
      HAKO_WORKLOAD_JOURNAL_BUCKET                = split(":::", var.workload_controller_runtime.journal_bucket_arn)[1]
      HAKO_WORKLOAD_JOURNAL_PREFIX                = var.workload_controller_runtime.journal_prefix
      HAKO_WORKLOAD_RECOVERY_LEASE_KEY            = trimprefix(var.workload_controller_runtime.recovery_lease_object_arn, "${var.workload_controller_runtime.journal_bucket_arn}/")
      HAKO_WORKLOAD_SOURCE_BUCKET                 = split(":::", var.workload_controller_runtime.source_bucket_arn)[1]
      HAKO_WORKLOAD_SOURCE_PREFIX                 = var.workload_controller_runtime.source_prefix
      HAKO_WORKLOAD_SOURCE_KMS_KEY_ID             = var.workload_controller_runtime.source_kms_key_arn
      HAKO_WORKLOAD_ARTIFACT_BUCKET               = split(":::", var.workload_controller_runtime.artifact_bucket_arn)[1]
      HAKO_WORKLOAD_ARTIFACT_PREFIX               = var.workload_controller_runtime.artifact_prefix
      HAKO_WORKLOAD_ARTIFACT_KMS_KEY_ID           = var.workload_controller_runtime.artifact_kms_key_arn
      HAKO_WORKLOAD_RESULT_QUEUE_URL              = aws_sqs_queue.workload_results[0].url
      HAKO_WORKLOAD_TICKET_KEY_ID                 = var.workload_controller_deployment.ticket_key_id
      HAKO_WORKLOAD_TICKET_PRIVATE_KEY_SECRET_ARN = var.workload_controller_deployment.ticket_private_key_secret_arn
      HAKO_WORKLOAD_RUNTIME_PROFILES_JSON         = var.workload_controller_deployment.runtime_profiles_json
      HAKO_WORKLOAD_RECOVERY_PAGE_SIZE            = tostring(var.workload_controller_deployment.recovery_page_size)
      HAKO_WORKLOAD_RECOVERY_MAX_PAGES            = tostring(var.workload_controller_deployment.recovery_max_pages)
      HAKO_WORKLOAD_RECOVERY_LEASE_TTL            = var.workload_controller_deployment.recovery_lease_ttl
    }
  }

  dynamic "vpc_config" {
    for_each = var.workload_controller_deployment == null ? [] : [var.workload_controller_vpc_config]
    content {
      subnet_ids         = vpc_config.value.subnet_ids
      security_group_ids = vpc_config.value.security_group_ids
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

# This optional Lambda is deliberately fake-runtime-only. The production
# Workspace runtime is not implemented, so the dev root must opt in explicitly.
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
