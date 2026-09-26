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
  evaluation_periods  = 2
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

resource "aws_cloudwatch_log_group" "fake_resource_controller" {
  count = var.enable_fake_resource_controller ? 1 : 0

  name              = "/aws/lambda/${var.resource_plane_id}-fake-controller"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:runtime" = "fake-only" })
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
