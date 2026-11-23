# AgentCore invocation orchestration is a separate Lambda consumer. The
# untrusted Coding Agent itself runs in the configured AgentCore Runtime.
resource "aws_cloudwatch_log_group" "agentcore_worker" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

  name              = "/aws/lambda/${var.resource_plane_id}-agentcore-worker"
  retention_in_days = var.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "agentcore-worker" })
}

data "aws_iam_policy_document" "agentcore_worker_assume_role" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

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
      values   = ["arn:${data.aws_partition.current.partition}:lambda:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:function:${var.resource_plane_id}-agentcore-worker"]
    }
  }
}

resource "aws_iam_role" "agentcore_worker" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

  name               = "${var.resource_plane_id}-agentcore-worker"
  assume_role_policy = data.aws_iam_policy_document.agentcore_worker_assume_role[0].json
  tags               = merge(local.common_tags, { "hako:component" = "agentcore-worker" })
}

resource "aws_iam_role_policy" "agentcore_worker" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-agentcore-worker-runtime"
  role = aws_iam_role.agentcore_worker[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ConsumeOnlyAgentCoreAttemptCommands"
        Effect   = "Allow"
        Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]
        Resource = aws_sqs_queue.agentcore_commands[0].arn
      },
      {
        Sid      = "PublishOnlyWorkloadRunResults"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = aws_sqs_queue.workload_results[0].arn
      },
      {
        Sid      = "ReadWriteOnlyAgentCoreAttemptJournal"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${var.agentcore_worker_deployment.journal_bucket_arn}/${var.agentcore_worker_deployment.journal_prefix}/*"
      },
      {
        Sid      = "ReadOnlyPinnedSourceArchives"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.agentcore_worker_deployment.source_bucket_arn}/${var.agentcore_worker_deployment.source_prefix}/*"
      },
      {
        Sid      = "WriteOnlyAttemptProposalArtifacts"
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
        Resource = "${var.agentcore_worker_deployment.artifact_bucket_arn}/${var.agentcore_worker_deployment.artifact_prefix}/*"
      },
      {
        Sid    = "ReadOnlyExactAgentTaskBrokerSecrets"
        Effect = "Allow"
        Action = ["secretsmanager:GetSecretValue"]
        Resource = [
          var.agentcore_worker_deployment.broker_client_cert_secret_arn,
          var.agentcore_worker_deployment.broker_client_key_secret_arn,
          var.agentcore_worker_deployment.broker_ca_secret_arn
        ]
      },
      {
        Sid      = "InvokeAndStopOnlyConfiguredAgentCoreRuntimes"
        Effect   = "Allow"
        Action   = ["bedrock-agentcore:InvokeAgentRuntime", "bedrock-agentcore:StopRuntimeSession"]
        Resource = var.agentcore_worker_deployment.runtime_arns
      },
      {
        Sid      = "WriteOnlyAgentCoreWorkerLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.agentcore_worker[0].arn}:*"
      }
      ], length(var.agentcore_worker_deployment.additional_secret_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyConfiguredAgentTaskSecretKeys"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.agentcore_worker_deployment.additional_secret_kms_key_arns
        }], var.agentcore_worker_deployment.journal_kms_key_arn == "" ? [] : [{
        Sid      = "UseOnlyAgentCoreJournalKey"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.agentcore_worker_deployment.journal_kms_key_arn
        }], var.agentcore_worker_deployment.source_kms_key_arn == "" ? [] : [{
        Sid      = "DecryptOnlyPinnedSourceArchives"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.agentcore_worker_deployment.source_kms_key_arn
        }], var.agentcore_worker_deployment.artifact_kms_key_arn == "" ? [] : [{
        Sid      = "EncryptOnlyAttemptProposalArtifacts"
        Effect   = "Allow"
        Action   = ["kms:GenerateDataKey"]
        Resource = var.agentcore_worker_deployment.artifact_kms_key_arn
        }], var.agentcore_worker_deployment.vpc_config == null ? [] : [{
        Sid    = "CreateLambdaVpcNetworkInterfaces"
        Effect = "Allow"
        Action = ["ec2:CreateNetworkInterface"]
        Resource = concat(
          ["arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:network-interface/*"],
          [for id in var.agentcore_worker_deployment.vpc_config.subnet_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:subnet/${id}"],
          [for id in var.agentcore_worker_deployment.vpc_config.security_group_ids : "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:security-group/${id}"]
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

resource "aws_lambda_function" "agentcore_worker" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

  function_name                  = "${var.resource_plane_id}-agentcore-worker"
  role                           = aws_iam_role.agentcore_worker[0].arn
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  filename                       = var.agentcore_worker_deployment.zip_path
  source_code_hash               = filebase64sha256(var.agentcore_worker_deployment.zip_path)
  memory_size                    = var.agentcore_worker_deployment.lambda_memory_size
  timeout                        = var.agentcore_worker_deployment.lambda_timeout_seconds
  reserved_concurrent_executions = var.agentcore_worker_deployment.reserved_concurrent_executions

  environment {
    variables = {
      HAKO_AGENTCORE_RESOURCE_PLANE_ID      = var.resource_plane_id
      HAKO_AGENTCORE_BROKER_URL             = var.agentcore_worker_deployment.broker_url
      HAKO_AGENTCORE_BROKER_CERT_SECRET_ARN = var.agentcore_worker_deployment.broker_client_cert_secret_arn
      HAKO_AGENTCORE_BROKER_KEY_SECRET_ARN  = var.agentcore_worker_deployment.broker_client_key_secret_arn
      HAKO_AGENTCORE_BROKER_CA_SECRET_ARN   = var.agentcore_worker_deployment.broker_ca_secret_arn
      HAKO_AGENTCORE_JOURNAL_BUCKET         = split(":::", var.agentcore_worker_deployment.journal_bucket_arn)[1]
      HAKO_AGENTCORE_JOURNAL_PREFIX         = var.agentcore_worker_deployment.journal_prefix
      HAKO_AGENTCORE_SOURCE_BUCKET          = split(":::", var.agentcore_worker_deployment.source_bucket_arn)[1]
      HAKO_AGENTCORE_SOURCE_PREFIX          = var.agentcore_worker_deployment.source_prefix
      HAKO_AGENTCORE_ARTIFACT_BUCKET        = split(":::", var.agentcore_worker_deployment.artifact_bucket_arn)[1]
      HAKO_AGENTCORE_ARTIFACT_PREFIX        = var.agentcore_worker_deployment.artifact_prefix
      HAKO_AGENTCORE_ARTIFACT_KMS_KEY_ID    = var.agentcore_worker_deployment.artifact_kms_key_arn
      HAKO_AGENTCORE_RESULT_QUEUE_URL       = aws_sqs_queue.workload_results[0].url
    }
  }

  dynamic "vpc_config" {
    for_each = var.agentcore_worker_deployment.vpc_config == null ? [] : [var.agentcore_worker_deployment.vpc_config]
    content {
      subnet_ids         = vpc_config.value.subnet_ids
      security_group_ids = vpc_config.value.security_group_ids
    }
  }

  depends_on = [aws_iam_role_policy.agentcore_worker]
  tags       = merge(local.common_tags, { "hako:component" = "agentcore-worker" })
}

resource "aws_lambda_event_source_mapping" "agentcore_worker" {
  count = var.agentcore_worker_deployment == null ? 0 : 1

  event_source_arn        = aws_sqs_queue.agentcore_commands[0].arn
  function_name           = aws_lambda_function.agentcore_worker[0].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  enabled                 = true
}
