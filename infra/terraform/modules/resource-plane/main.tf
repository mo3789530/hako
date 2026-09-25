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
    sid       = "WriteControllerLogs"
    effect    = "Allow"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.resource_controller.arn}:*"]
  }
}

resource "aws_iam_role_policy" "resource_controller" {
  name   = "${var.resource_plane_id}-resource-controller-runtime"
  role   = aws_iam_role.resource_controller.id
  policy = data.aws_iam_policy_document.resource_controller.json
}
