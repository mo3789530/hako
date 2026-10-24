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
    "hako:managed-by" = "hako"
    "hako:plane"      = "control"
  })

  dispatcher_resource_planes = var.enable_outbox_dispatcher ? try(jsondecode(var.resource_plane_manifest_json).resource_planes, []) : []
  dispatcher_command_queue_arns = [
    for plane in local.dispatcher_resource_planes : format(
      "arn:%s:sqs:%s:%s:%s",
      data.aws_partition.current.partition,
      plane.region,
      plane.account_id,
      element(split("/", plane.command_queue_url), length(split("/", plane.command_queue_url)) - 1)
    )
  ]
  dispatcher_workload_command_queue_arns = [
    for plane in local.dispatcher_resource_planes : format(
      "arn:%s:sqs:%s:%s:%s",
      data.aws_partition.current.partition,
      plane.region,
      plane.account_id,
      element(split("/", plane.workload_command_queue_url), length(split("/", plane.workload_command_queue_url)) - 1)
    ) if try(trimspace(plane.workload_command_queue_url), "") != ""
  ]
  dispatcher_actions_runner_queue_arns = [
    for plane in local.dispatcher_resource_planes : format(
      "arn:%s:sqs:%s:%s:%s",
      data.aws_partition.current.partition,
      plane.region,
      plane.account_id,
      element(split("/", plane.actions_runner_startup_queue_url), length(split("/", plane.actions_runner_startup_queue_url)) - 1)
    ) if try(trimspace(plane.actions_runner_startup_queue_url), "") != ""
  ]
  dispatcher_workload_queue_arns = var.enable_outbox_dispatcher ? [aws_sqs_queue.workload_scheduler[0].arn] : []
  dispatcher_github_checks_queue_arns = var.enable_github_checks_delivery ? [
    aws_sqs_queue.github_checks[0].arn
  ] : []

  dispatcher_iam_statements = concat(
    [
      {
        Sid      = "WriteFunctionLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.outbox_dispatcher.arn}:*"
      },
      {
        Sid      = "ConnectAsHakoDispatcher"
        Effect   = "Allow"
        Action   = ["dsql:DbConnect"]
        Resource = aws_dsql_cluster.control_plane.arn
      }
    ],
    length(local.dispatcher_command_queue_arns) > 0 ? [
      {
        Sid      = "PublishCommandsToRegisteredPlanes"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = local.dispatcher_command_queue_arns
      }
    ] : [],
    length(local.dispatcher_workload_command_queue_arns) > 0 ? [
      {
        Sid      = "PublishWorkloadCommandsToRegisteredPlanes"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = local.dispatcher_workload_command_queue_arns
      }
    ] : [],
    length(local.dispatcher_actions_runner_queue_arns) > 0 ? [
      {
        Sid      = "PublishActionsRunnerStartupToDedicatedQueue"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = local.dispatcher_actions_runner_queue_arns
      }
    ] : [],
    length(local.dispatcher_workload_queue_arns) > 0 ? [
      {
        Sid      = "PublishWorkloadsToSchedulerQueue"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = local.dispatcher_workload_queue_arns
      }
    ] : [],
    length(local.dispatcher_github_checks_queue_arns) > 0 ? [
      {
        Sid      = "PublishGitHubChecks"
        Effect   = "Allow"
        Action   = ["sqs:SendMessage"]
        Resource = local.dispatcher_github_checks_queue_arns
      }
    ] : []
  )
}

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}

locals {
  api_lambda_image_enabled = trimspace(var.api_lambda_image_uri) != ""
  api_lambda_image_active  = var.api_lambda_image_active && local.api_lambda_image_enabled
}

resource "aws_dsql_cluster" "control_plane" {
  deletion_protection_enabled = var.dsql_deletion_protection_enabled

  tags = local.common_tags
}

# HTTP API stage metrics are emitted by API Gateway without route-level detailed
# metrics. Keep alarms at API/stage granularity to avoid cardinality and cost.
resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${var.api_name}-api-5xx"
  alarm_description   = "HTTP API returned more than five server errors in one minute. Inspect API and Lambda logs using X-Hako-Request-ID."
  namespace           = "AWS/ApiGateway"
  metric_name         = "5xx"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    ApiId = aws_apigatewayv2_api.control_plane.id
    Stage = aws_apigatewayv2_stage.default.name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "api_latency_p95" {
  alarm_name          = "${var.api_name}-api-latency-p95"
  alarm_description   = "HTTP API p95 end-to-end latency exceeded two seconds for five consecutive minutes."
  namespace           = "AWS/ApiGateway"
  metric_name         = "Latency"
  extended_statistic  = "p95"
  period              = 60
  evaluation_periods  = 5
  threshold           = 2000
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    ApiId = aws_apigatewayv2_api.control_plane.id
    Stage = aws_apigatewayv2_stage.default.name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "dsql_query_timeouts" {
  alarm_name          = "${var.api_name}-dsql-query-timeouts"
  alarm_description   = "Aurora DSQL recorded a query timeout. Inspect database load and correlated Control Plane requests."
  namespace           = "AWS/AuroraDSQL"
  metric_name         = "QueryTimeouts"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { ClusterId = aws_dsql_cluster.control_plane.identifier }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "dsql_occ_conflicts" {
  alarm_name          = "${var.api_name}-dsql-occ-conflicts"
  alarm_description   = "Aurora DSQL recorded sustained OCC conflicts. Review hot-key transactions and retry behavior."
  namespace           = "AWS/AuroraDSQL"
  metric_name         = "OccConflicts"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 5
  threshold           = 10
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { ClusterId = aws_dsql_cluster.control_plane.identifier }
  tags                = local.common_tags
}

resource "aws_sqs_queue" "workload_scheduler_dlq" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  name                      = "${var.api_name}-workload-scheduler-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
  tags                      = local.common_tags
}

resource "aws_sqs_queue" "workload_scheduler" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  name                       = "${var.api_name}-workload-scheduler"
  visibility_timeout_seconds = 120
  message_retention_seconds  = 345600
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.workload_scheduler_dlq[0].arn
    maxReceiveCount     = 5
  })
  tags = local.common_tags
}

resource "aws_sqs_queue" "github_checks_dlq" {
  count = var.enable_github_checks_delivery ? 1 : 0

  name                      = "${var.api_name}-github-checks-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.common_tags, { "hako:queue-role" = "github-checks-dlq" })
}

resource "aws_sqs_queue" "github_checks" {
  count = var.enable_github_checks_delivery ? 1 : 0

  name                       = "${var.api_name}-github-checks"
  visibility_timeout_seconds = var.github_checks_queue_visibility_timeout_seconds
  message_retention_seconds  = 345600
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  tags                       = merge(local.common_tags, { "hako:queue-role" = "github-checks" })
}

resource "aws_sqs_queue_redrive_policy" "github_checks" {
  count = var.enable_github_checks_delivery ? 1 : 0

  queue_url = aws_sqs_queue.github_checks[0].id
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.github_checks_dlq[0].arn
    maxReceiveCount     = 5
  })
}

resource "aws_sqs_queue_redrive_allow_policy" "github_checks_dlq" {
  count = var.enable_github_checks_delivery ? 1 : 0

  queue_url = aws_sqs_queue.github_checks_dlq[0].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.github_checks[0].arn]
  })
}

resource "aws_iam_policy" "github_checks_consumer" {
  count = var.enable_github_checks_delivery ? 1 : 0

  name        = "${var.api_name}-github-checks-consumer"
  description = "Receive and acknowledge Hako GitHub Checks messages from only the dedicated queue."
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "sqs:ChangeMessageVisibility",
        "sqs:DeleteMessage",
        "sqs:GetQueueAttributes",
        "sqs:GetQueueUrl",
        "sqs:ReceiveMessage",
      ]
      Resource = aws_sqs_queue.github_checks[0].arn
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role" "github_checks_consumer" {
  name = "${var.api_name}-github-checks-consumer"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        ArnLike      = { "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:ecs:${var.aws_region}:${data.aws_caller_identity.current.account_id}:*" }
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
  tags = merge(local.common_tags, { "hako:role-purpose" = "github-checks-consumer-task" })
}

resource "aws_iam_policy" "github_checks_dsql" {
  count = var.enable_github_checks_delivery ? 1 : 0

  name        = "${var.api_name}-github-checks-dsql"
  description = "Connect the Hako GitHub Checks consumer to the Control Plane DSQL cluster."
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["dsql:DbConnect"]
      Resource = aws_dsql_cluster.control_plane.arn
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "github_checks_consumer_queue" {
  count = var.enable_github_checks_delivery ? 1 : 0

  role       = aws_iam_role.github_checks_consumer.name
  policy_arn = aws_iam_policy.github_checks_consumer[0].arn
}

resource "aws_iam_role_policy_attachment" "github_checks_consumer_dsql" {
  count = var.enable_github_checks_delivery ? 1 : 0

  role       = aws_iam_role.github_checks_consumer.name
  policy_arn = aws_iam_policy.github_checks_dsql[0].arn
}

resource "aws_cloudwatch_metric_alarm" "github_checks_queue_age" {
  count = var.enable_github_checks_delivery ? 1 : 0

  alarm_name          = "${var.api_name}-github-checks-queue-age"
  alarm_description   = "GitHub Checks deliveries are waiting too long for a consumer."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.github_checks_queue_age_alarm_threshold_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.github_checks_alarm_actions
  ok_actions          = var.github_checks_alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.github_checks[0].name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "github_checks_queue_depth" {
  count = var.enable_github_checks_delivery ? 1 : 0

  alarm_name          = "${var.api_name}-github-checks-queue-depth"
  alarm_description   = "The number of visible GitHub Checks deliveries exceeds the configured threshold."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.github_checks_queue_depth_alarm_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.github_checks_alarm_actions
  ok_actions          = var.github_checks_alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.github_checks[0].name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "github_checks_dlq_messages" {
  count = var.enable_github_checks_delivery ? 1 : 0

  alarm_name          = "${var.api_name}-github-checks-dlq-messages"
  alarm_description   = "One or more GitHub Checks deliveries reached the dead-letter queue."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.github_checks_alarm_actions
  ok_actions          = var.github_checks_alarm_actions
  dimensions          = { QueueName = aws_sqs_queue.github_checks_dlq[0].name }
  tags                = local.common_tags
}

resource "aws_iam_role" "github_checks_consumer_execution" {
  count = var.enable_github_checks_consumer_service ? 1 : 0

  name = "${var.api_name}-github-checks-execution"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        ArnLike      = { "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:ecs:${var.aws_region}:${data.aws_caller_identity.current.account_id}:*" }
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
  tags = merge(local.common_tags, { "hako:role-purpose" = "github-checks-task-execution" })
}

resource "aws_iam_role_policy" "github_checks_consumer_execution_permissions" {
  count = var.enable_github_checks_consumer_service ? 1 : 0

  name = "${var.api_name}-github-checks-execution"
  role = aws_iam_role.github_checks_consumer_execution[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "AuthenticateToECR"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "PullOnlyChecksConsumerImage"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
        ]
        Resource = var.github_checks_consumer_ecr_repository_arn
      },
      {
        Sid      = "WriteOnlyChecksConsumerLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.github_checks_consumer[0].arn}:*"
      },
      {
        Sid      = "ReadOnlyBrokerMTLSSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = [var.github_checks_broker_client_cert_secret_arn, var.github_checks_broker_client_key_secret_arn, var.github_checks_broker_ca_secret_arn]
      }
      ], var.github_checks_broker_kms_key_arn == "" ? [] : [{
        Sid      = "DecryptBrokerMTLSSecrets"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.github_checks_broker_kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${var.aws_region}.${data.aws_partition.current.dns_suffix}"
            "kms:EncryptionContext:SecretARN" = [
              var.github_checks_broker_client_cert_secret_arn,
              var.github_checks_broker_client_key_secret_arn,
              var.github_checks_broker_ca_secret_arn,
            ]
          }
        }
    }])
  })
}

resource "aws_cloudwatch_log_group" "github_checks_consumer" {
  count = var.enable_github_checks_consumer_service ? 1 : 0

  name              = "/ecs/${var.api_name}-github-checks-consumer"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_ecs_task_definition" "github_checks_consumer" {
  count = var.enable_github_checks_consumer_service ? 1 : 0

  family                   = "${var.api_name}-github-checks-consumer"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.github_checks_consumer_cpu)
  memory                   = tostring(var.github_checks_consumer_memory)
  execution_role_arn       = aws_iam_role.github_checks_consumer_execution[0].arn
  task_role_arn            = aws_iam_role.github_checks_consumer.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }
  container_definitions = jsonencode([{
    name                   = "github-checks-consumer"
    image                  = var.github_checks_consumer_image_uri
    essential              = true
    readonlyRootFilesystem = true
    stopTimeout            = 60
    environment = [
      { name = "AWS_REGION", value = var.aws_region },
      { name = "HAKO_GITHUB_CHECKS_QUEUE_URL", value = aws_sqs_queue.github_checks[0].url },
      { name = "HAKO_GITHUB_CHECKS_QUEUE_REGION", value = var.aws_region },
      { name = "HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS", value = tostring(var.github_checks_queue_visibility_timeout_seconds) },
      { name = "HAKO_GITHUB_CHECKS_BROKER_URL", value = var.github_checks_broker_url },
      { name = "HAKO_DSQL_HOST", value = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws" },
      { name = "HAKO_DSQL_USER", value = "hako_checks_consumer" },
      { name = "HAKO_DSQL_DATABASE", value = var.github_checks_dsql_database },
    ]
    secrets = [
      { name = "HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM", valueFrom = var.github_checks_broker_client_cert_secret_arn },
      { name = "HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM", valueFrom = var.github_checks_broker_client_key_secret_arn },
      { name = "HAKO_GITHUB_CHECKS_BROKER_CA_PEM", valueFrom = var.github_checks_broker_ca_secret_arn },
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.github_checks_consumer[0].name
        "awslogs-region"        = var.aws_region
        "awslogs-stream-prefix" = "consumer"
      }
    }
  }])
  tags = local.common_tags
}

resource "aws_ecs_service" "github_checks_consumer" {
  count = var.enable_github_checks_consumer_service ? 1 : 0

  name                               = "${var.api_name}-github-checks-consumer"
  cluster                            = var.github_checks_consumer_cluster_arn
  task_definition                    = aws_ecs_task_definition.github_checks_consumer[0].arn
  desired_count                      = var.github_checks_consumer_desired_count
  launch_type                        = "FARGATE"
  platform_version                   = "LATEST"
  enable_execute_command             = false
  enable_ecs_managed_tags            = true
  propagate_tags                     = "SERVICE"
  deployment_maximum_percent         = 200
  deployment_minimum_healthy_percent = 50

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.github_checks_consumer_subnet_ids
    security_groups  = var.github_checks_consumer_security_group_ids
    assign_public_ip = false
  }

  tags = local.common_tags
}

resource "aws_iam_role" "api_lambda" {
  name = "${var.api_name}-api-lambda"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_cloudwatch_log_group" "api_lambda" {
  name              = "/aws/lambda/${var.api_name}-api"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "api_lambda_image" {
  count             = local.api_lambda_image_enabled ? 1 : 0
  name              = "/aws/lambda/${var.api_name}-api-image"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_ecr_repository" "api_lambda" {
  name                 = "${var.api_name}-api-lambda"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = false

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = local.common_tags
}

resource "aws_ecr_lifecycle_policy" "api_lambda" {
  repository = aws_ecr_repository.api_lambda.name
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged build artifacts after seven days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      }
    ]
  })
}

resource "aws_ecr_repository_policy" "api_lambda" {
  repository = aws_ecr_repository.api_lambda.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "LambdaImagePull"
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:lambda:${var.aws_region}:${data.aws_caller_identity.current.account_id}:function:${var.api_name}-api-image" }
      }
    }]
  })
}

resource "aws_iam_role_policy" "api_lambda" {
  name = "${var.api_name}-api-runtime"
  role = aws_iam_role.api_lambda.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid    = "WriteFunctionLogs"
        Effect = "Allow"
        Action = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = concat(
          ["${aws_cloudwatch_log_group.api_lambda.arn}:*"],
          local.api_lambda_image_enabled ? ["${aws_cloudwatch_log_group.api_lambda_image[0].arn}:*"] : []
        )
      },
      {
        Sid      = "ConnectAsHakoAPI"
        Effect   = "Allow"
        Action   = ["dsql:DbConnect"]
        Resource = aws_dsql_cluster.control_plane.arn
      }
      ], var.github_webhook_secret_arn == "" ? [] : [{
        Sid      = "ReadGitHubWebhookSecret"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.github_webhook_secret_arn
        }], !var.github_app_setup_enabled ? [] : [{
        Sid      = "ReadGitHubAppOAuthClientSecret"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.github_app_client_secret_arn
        }], !var.tenant_github_webhook_enabled ? [] : [{
        Sid      = "AssumeApprovedTenantWebhookReaders"
        Effect   = "Allow"
        Action   = ["sts:AssumeRole"]
        Resource = sort(tolist(var.tenant_github_webhook_reader_role_arns))
        }], length(var.resource_plane_health_report_secret_arns) == 0 ? [] : [{
        Sid      = "ReadConfiguredResourcePlaneHealthReportSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = sort(values(var.resource_plane_health_report_secret_arns))
        }], length(var.resource_plane_workload_spec_secret_arns) == 0 ? [] : [{
        Sid      = "ReadConfiguredResourcePlaneWorkloadSpecSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = sort(values(var.resource_plane_workload_spec_secret_arns))
        }], length(var.resource_plane_artifact_cleanup_secret_arns) == 0 ? [] : [{
        Sid      = "ReadConfiguredResourcePlaneArtifactCleanupSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = sort(values(var.resource_plane_artifact_cleanup_secret_arns))
    }])
  })
}

module "github_broker_tenant_iam" {
  source = "../github-broker-tenant-iam"

  broker_role_name                    = var.github_broker_role_name
  policy_name_prefix                  = var.api_name
  tenant_private_key_reader_role_arns = var.tenant_github_private_key_reader_role_arns
  tenant_webhook_reader_role_arns     = var.tenant_github_webhook_reader_role_arns
}

resource "aws_lambda_function" "api" {
  function_name    = "${var.api_name}-api"
  role             = aws_iam_role.api_lambda.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = var.api_lambda_zip_path
  source_code_hash = filebase64sha256(var.api_lambda_zip_path)
  memory_size      = var.api_lambda_memory_size
  timeout          = var.api_lambda_timeout_seconds

  environment {
    variables = {
      HAKO_COGNITO_ISSUER                              = var.cognito_issuer_url
      HAKO_COGNITO_CLIENT_ID                           = aws_cognito_user_pool_client.cli.id
      HAKO_DSQL_HOST                                   = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                                   = "hako_api"
      HAKO_DSQL_DATABASE                               = "postgres"
      HAKO_DEFAULT_WORKSPACE_IMAGE                     = var.default_workspace_image
      HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS             = var.default_workspace_runtime_class
      HAKO_WORKLOAD_RUNS_ENABLED                       = tostring(var.enable_workload_run_creation)
      HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED           = tostring(var.enable_webhook_workload_triggers)
      HAKO_GITHUB_APP_MANAGEMENT_ENABLED               = tostring(var.enable_webhook_workload_triggers)
      HAKO_GITHUB_WEBHOOK_SECRET_ARN                   = var.github_webhook_secret_arn
      HAKO_TENANT_GITHUB_WEBHOOK_ENABLED               = tostring(var.tenant_github_webhook_enabled)
      HAKO_GITHUB_APP_SLUG                             = var.github_app_setup_enabled ? var.github_app_slug : ""
      HAKO_GITHUB_APP_ID                               = var.github_app_setup_enabled ? tostring(var.github_app_id) : ""
      HAKO_GITHUB_APP_CLIENT_ID                        = var.github_app_setup_enabled ? var.github_app_client_id : ""
      HAKO_GITHUB_APP_CLIENT_SECRET_ARN                = var.github_app_setup_enabled ? var.github_app_client_secret_arn : ""
      HAKO_GITHUB_APP_CALLBACK_URL                     = var.github_app_setup_enabled ? var.github_app_callback_url : ""
      HAKO_RESOURCE_PLANE_HEALTH_REPORT_SECRET_ARNS    = jsonencode(var.resource_plane_health_report_secret_arns)
      HAKO_RESOURCE_PLANE_WORKLOAD_SPEC_SECRET_ARNS    = jsonencode(var.resource_plane_workload_spec_secret_arns)
      HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS = jsonencode(var.resource_plane_artifact_cleanup_secret_arns)
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.api_lambda,
    aws_iam_role_policy.api_lambda,
  ]

  tags = local.common_tags
}

# Keep the ZIP function as the independently deployable rollback target. The
# API Gateway integration moves to this candidate only when a digest is given.
resource "aws_lambda_function" "api_image" {
  count         = local.api_lambda_image_enabled ? 1 : 0
  function_name = "${var.api_name}-api-image"
  role          = aws_iam_role.api_lambda.arn
  package_type  = "Image"
  image_uri     = var.api_lambda_image_uri
  architectures = ["arm64"]
  memory_size   = var.api_lambda_memory_size
  timeout       = var.api_lambda_timeout_seconds

  environment {
    variables = {
      HAKO_COGNITO_ISSUER                              = var.cognito_issuer_url
      HAKO_COGNITO_CLIENT_ID                           = aws_cognito_user_pool_client.cli.id
      HAKO_DSQL_HOST                                   = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                                   = "hako_api"
      HAKO_DSQL_DATABASE                               = "postgres"
      HAKO_DEFAULT_WORKSPACE_IMAGE                     = var.default_workspace_image
      HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS             = var.default_workspace_runtime_class
      HAKO_WORKLOAD_RUNS_ENABLED                       = tostring(var.enable_workload_run_creation)
      HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED           = tostring(var.enable_webhook_workload_triggers)
      HAKO_GITHUB_APP_MANAGEMENT_ENABLED               = tostring(var.enable_webhook_workload_triggers)
      HAKO_API_HTTP_MODE                               = "true"
      HAKO_GITHUB_WEBHOOK_SECRET_ARN                   = var.github_webhook_secret_arn
      HAKO_TENANT_GITHUB_WEBHOOK_ENABLED               = tostring(var.tenant_github_webhook_enabled)
      HAKO_GITHUB_APP_SLUG                             = var.github_app_setup_enabled ? var.github_app_slug : ""
      HAKO_GITHUB_APP_ID                               = var.github_app_setup_enabled ? tostring(var.github_app_id) : ""
      HAKO_GITHUB_APP_CLIENT_ID                        = var.github_app_setup_enabled ? var.github_app_client_id : ""
      HAKO_GITHUB_APP_CLIENT_SECRET_ARN                = var.github_app_setup_enabled ? var.github_app_client_secret_arn : ""
      HAKO_GITHUB_APP_CALLBACK_URL                     = var.github_app_setup_enabled ? var.github_app_callback_url : ""
      HAKO_RESOURCE_PLANE_HEALTH_REPORT_SECRET_ARNS    = jsonencode(var.resource_plane_health_report_secret_arns)
      HAKO_RESOURCE_PLANE_WORKLOAD_SPEC_SECRET_ARNS    = jsonencode(var.resource_plane_workload_spec_secret_arns)
      HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS = jsonencode(var.resource_plane_artifact_cleanup_secret_arns)
    }
  }

  lifecycle {
    precondition {
      condition     = startswith(var.api_lambda_image_uri, "${aws_ecr_repository.api_lambda.repository_url}@sha256:")
      error_message = "api_lambda_image_uri must point to the module-managed ECR repository and use an immutable sha256 digest."
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.api_lambda_image,
    aws_iam_role_policy.api_lambda,
    aws_ecr_repository_policy.api_lambda,
  ]

  tags = local.common_tags
}

resource "aws_iam_policy" "dsql_migration_admin" {
  name        = "${var.api_name}-dsql-migration-admin"
  description = "Connect to the Hako Control Plane DSQL cluster as the admin database role for migrations/bootstrap only."
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["dsql:DbConnectAdmin"]
      Resource = aws_dsql_cluster.control_plane.arn
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role" "outbox_dispatcher" {
  name = "${var.api_name}-outbox-dispatcher"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy" "outbox_dispatcher" {
  name = "${var.api_name}-outbox-dispatcher-runtime"
  role = aws_iam_role.outbox_dispatcher.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.dispatcher_iam_statements
  })
}

resource "aws_cloudwatch_log_group" "outbox_dispatcher" {
  name              = "/aws/lambda/${var.api_name}-outbox-dispatcher"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_lambda_function" "outbox_dispatcher" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  function_name    = "${var.api_name}-outbox-dispatcher"
  role             = aws_iam_role.outbox_dispatcher.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = var.dispatcher_lambda_zip_path
  source_code_hash = filebase64sha256(var.dispatcher_lambda_zip_path)
  memory_size      = 512
  timeout          = 60

  environment {
    variables = {
      HAKO_DSQL_HOST                    = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                    = "hako_dispatcher"
      HAKO_DSQL_DATABASE                = "postgres"
      HAKO_RESOURCE_PLANE_MANIFEST_JSON = var.resource_plane_manifest_json
      HAKO_WORKLOAD_SCHEDULER_QUEUE_URL = aws_sqs_queue.workload_scheduler[0].url
      HAKO_GITHUB_CHECKS_QUEUE_URL      = var.enable_github_checks_delivery ? aws_sqs_queue.github_checks[0].url : ""
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.outbox_dispatcher,
    aws_iam_role_policy.outbox_dispatcher,
  ]

  tags = local.common_tags
}

resource "aws_cloudwatch_event_rule" "outbox_dispatcher" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  name                = "${var.api_name}-outbox-dispatcher"
  description         = "Poll the Hako transactional Outbox and publish due commands."
  schedule_expression = "rate(1 minute)"
  state               = "ENABLED"
  tags                = local.common_tags
}

resource "aws_cloudwatch_event_target" "outbox_dispatcher" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  rule      = aws_cloudwatch_event_rule.outbox_dispatcher[0].name
  target_id = "hako-outbox-dispatcher"
  arn       = aws_lambda_function.outbox_dispatcher[0].arn

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "outbox_dispatcher_eventbridge" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  statement_id  = "AllowOutboxDispatcherSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.outbox_dispatcher[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.outbox_dispatcher[0].arn
}

resource "aws_cloudwatch_metric_alarm" "outbox_dispatcher_errors" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  alarm_name          = "${var.api_name}-outbox-dispatcher-errors"
  alarm_description   = "The Hako Outbox Dispatcher Lambda reported invocation errors. Check logs and durable Outbox lease state."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { FunctionName = aws_lambda_function.outbox_dispatcher[0].function_name }
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "outbox_oldest_unpublished_age" {
  count = var.enable_outbox_dispatcher ? 1 : 0

  alarm_name          = "${var.api_name}-outbox-oldest-unpublished-age"
  alarm_description   = "The oldest unpublished Control Plane Outbox event is over five minutes old for two consecutive periods. Check Dispatcher logs and durable leases."
  namespace           = "Hako/ControlPlane"
  metric_name         = "OutboxOldestUnpublishedAgeSeconds"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 300
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { Service = "OutboxDispatcher" }
  tags                = local.common_tags
}

resource "aws_iam_role" "github_webhook_processor" {
  name = "${var.api_name}-github-webhook-processor"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy" "github_webhook_processor" {
  count = var.enable_github_webhook_processor ? 1 : 0

  name = "${var.api_name}-github-webhook-processor-runtime"
  role = aws_iam_role.github_webhook_processor.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteFunctionLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.github_webhook_processor[0].arn}:*"
      },
      {
        Sid      = "ConnectAsHakoWebhookProcessor"
        Effect   = "Allow"
        Action   = ["dsql:DbConnect"]
        Resource = aws_dsql_cluster.control_plane.arn
      }
    ]
  })
}

resource "aws_cloudwatch_log_group" "github_webhook_processor" {
  count = var.enable_github_webhook_processor ? 1 : 0

  name              = "/aws/lambda/${var.api_name}-github-webhook-processor"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_lambda_function" "github_webhook_processor" {
  count = var.enable_github_webhook_processor ? 1 : 0

  function_name    = "${var.api_name}-github-webhook-processor"
  role             = aws_iam_role.github_webhook_processor.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = var.github_webhook_processor_lambda_zip_path
  source_code_hash = filebase64sha256(var.github_webhook_processor_lambda_zip_path)
  memory_size      = 512
  timeout          = 60

  environment {
    variables = {
      HAKO_DSQL_HOST                         = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                         = "hako_webhook_processor"
      HAKO_DSQL_DATABASE                     = "postgres"
      HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED = tostring(var.enable_webhook_workload_triggers)
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.github_webhook_processor,
    aws_iam_role_policy.github_webhook_processor,
  ]

  tags = local.common_tags
}

resource "aws_cloudwatch_event_rule" "github_webhook_processor" {
  count = var.enable_github_webhook_processor ? 1 : 0

  name                = "${var.api_name}-github-webhook-processor"
  description         = "Drain pending GitHub Installation repository events from the Hako webhook inbox."
  schedule_expression = "rate(1 minute)"
  state               = "ENABLED"
  tags                = local.common_tags
}

resource "aws_cloudwatch_event_target" "github_webhook_processor" {
  count = var.enable_github_webhook_processor ? 1 : 0

  rule      = aws_cloudwatch_event_rule.github_webhook_processor[0].name
  target_id = "hako-github-webhook-processor"
  arn       = aws_lambda_function.github_webhook_processor[0].arn

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "github_webhook_processor_eventbridge" {
  count = var.enable_github_webhook_processor ? 1 : 0

  statement_id  = "AllowGitHubWebhookProcessorSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.github_webhook_processor[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.github_webhook_processor[0].arn
}

resource "aws_cloudwatch_metric_alarm" "github_webhook_processor_errors" {
  count = var.enable_github_webhook_processor ? 1 : 0

  alarm_name          = "${var.api_name}-github-webhook-processor-errors"
  alarm_description   = "The Hako GitHub Webhook Processor reported invocation errors. Check logs and pending inbox deliveries."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { FunctionName = aws_lambda_function.github_webhook_processor[0].function_name }
  tags                = local.common_tags
}

resource "aws_iam_role" "workspace_idle_reconciler" {
  name = "${var.api_name}-workspace-idle-reconciler"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.common_tags
}

resource "aws_iam_role_policy" "workspace_idle_reconciler" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  name = "${var.api_name}-workspace-idle-reconciler-runtime"
  role = aws_iam_role.workspace_idle_reconciler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteFunctionLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.workspace_idle_reconciler[0].arn}:*"
      },
      {
        Sid      = "ConnectAsWorkspaceIdleReconciler"
        Effect   = "Allow"
        Action   = ["dsql:DbConnect"]
        Resource = aws_dsql_cluster.control_plane.arn
      }
    ]
  })
}

resource "aws_cloudwatch_log_group" "workspace_idle_reconciler" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  name              = "/aws/lambda/${var.api_name}-workspace-idle-reconciler"
  retention_in_days = var.lambda_log_retention_days
  tags              = local.common_tags
}

resource "aws_lambda_function" "workspace_idle_reconciler" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  function_name    = "${var.api_name}-workspace-idle-reconciler"
  role             = aws_iam_role.workspace_idle_reconciler.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = var.workspace_idle_reconciler_lambda_zip_path
  source_code_hash = filebase64sha256(var.workspace_idle_reconciler_lambda_zip_path)
  memory_size      = 256
  timeout          = 60

  environment {
    variables = {
      HAKO_DSQL_HOST                            = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                            = "hako_workspace_idle_reconciler"
      HAKO_DSQL_DATABASE                        = "postgres"
      HAKO_WORKSPACE_IDLE_RECONCILER_BATCH_SIZE = tostring(var.workspace_idle_reconciler_batch_size)
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.workspace_idle_reconciler,
    aws_iam_role_policy.workspace_idle_reconciler,
  ]

  tags = local.common_tags
}

resource "aws_cloudwatch_event_rule" "workspace_idle_reconciler" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  name                = "${var.api_name}-workspace-idle-reconciler"
  description         = "Find opt-in Workspaces past their connection grace period and request fenced suspension."
  schedule_expression = var.workspace_idle_reconciler_schedule_expression
  state               = "ENABLED"
  tags                = local.common_tags
}

resource "aws_cloudwatch_event_target" "workspace_idle_reconciler" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  rule      = aws_cloudwatch_event_rule.workspace_idle_reconciler[0].name
  target_id = "hako-workspace-idle-reconciler"
  arn       = aws_lambda_function.workspace_idle_reconciler[0].arn

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 2
  }
}

resource "aws_lambda_permission" "workspace_idle_reconciler_eventbridge" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  statement_id  = "AllowWorkspaceIdleReconcilerSchedule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.workspace_idle_reconciler[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.workspace_idle_reconciler[0].arn
}

resource "aws_cloudwatch_metric_alarm" "workspace_idle_reconciler_errors" {
  count = var.enable_workspace_idle_reconciler ? 1 : 0

  alarm_name          = "${var.api_name}-workspace-idle-reconciler-errors"
  alarm_description   = "The Workspace idle reconciler Lambda failed. Check its logs and pending Outbox Operations."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { FunctionName = aws_lambda_function.workspace_idle_reconciler[0].function_name }
  tags                = local.common_tags
}

resource "aws_apigatewayv2_api" "control_plane" {
  name          = var.api_name
  protocol_type = "HTTP"

  tags = local.common_tags
}

resource "aws_apigatewayv2_authorizer" "cognito" {
  api_id           = aws_apigatewayv2_api.control_plane.id
  name             = "hako-cognito-jwt"
  authorizer_type  = "JWT"
  identity_sources = ["$request.header.Authorization"]

  jwt_configuration {
    audience = [aws_cognito_user_pool_client.cli.id]
    issuer   = var.cognito_issuer_url
  }
}

resource "aws_apigatewayv2_integration" "api_lambda" {
  api_id                 = aws_apigatewayv2_api.control_plane.id
  integration_type       = "AWS_PROXY"
  integration_method     = "POST"
  integration_uri        = local.api_lambda_image_active ? aws_lambda_function.api_image[0].invoke_arn : aws_lambda_function.api.invoke_arn
  payload_format_version = "2.0"
  timeout_milliseconds   = 29000
}

resource "aws_apigatewayv2_route" "api_lambda" {
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "$default"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "JWT"
  authorizer_id      = aws_apigatewayv2_authorizer.cognito.id
  authorization_scopes = [
    aws_cognito_resource_server.hako_api.scope_identifiers[0],
  ]
}

# Minimal unauthenticated liveness route for load balancers and operations.
# All other paths continue through the JWT-protected $default route.
resource "aws_apigatewayv2_route" "public_healthz" {
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "GET /healthz"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

# GitHub authenticates webhook requests using the configured raw-body HMAC;
# this path deliberately bypasses the Cognito JWT default route.
resource "aws_apigatewayv2_route" "github_webhook" {
  count              = var.github_webhook_secret_arn == "" ? 0 : 1
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "POST /v1/integrations/github/webhook"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

# Tenant-owned webhook routes authenticate with a route-resolved Webhook-only
# secret reader; they never share the legacy App's secret or Cognito JWT.
resource "aws_apigatewayv2_route" "tenant_github_webhook" {
  count              = var.tenant_github_webhook_enabled ? 1 : 0
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "POST /v1/integrations/github/apps/{integration_id}/webhook"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

# This route intentionally bypasses Cognito only when a finite, exact
# Resource Plane ID -> HMAC Secret ARN map is configured. The API still
# authenticates every request using that Plane-specific signature.
resource "aws_apigatewayv2_route" "resource_plane_health_report" {
  count = length(var.resource_plane_health_report_secret_arns) == 0 ? 0 : 1

  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "POST /v1/internal/resource-planes/{resource_plane_id}/health"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

# Resource Plane execution metadata is returned only after a dedicated
# per-Plane HMAC signature and live Run/attempt/Installation checks.
resource "aws_apigatewayv2_route" "resource_plane_workload_spec" {
  count = length(var.resource_plane_workload_spec_secret_arns) == 0 ? 0 : 1

  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "GET /v1/internal/resource-planes/{resource_plane_id}/tenants/{tenant_id}/workloads/{workload_run_id}/attempts/{attempt_id}/generations/{generation}/specification"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

# GitHub's setup redirect carries an OAuth code plus our one-time state. Hako
# exchanges the code and verifies the installing user before binding a Tenant.
resource "aws_apigatewayv2_route" "github_setup_callback" {
  count              = var.github_app_setup_enabled ? 1 : 0
  api_id             = aws_apigatewayv2_api.control_plane.id
  route_key          = "GET /v1/integrations/github/setup/callback"
  target             = "integrations/${aws_apigatewayv2_integration.api_lambda.id}"
  authorization_type = "NONE"
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.control_plane.id
  name        = "$default"
  auto_deploy = true

  dynamic "route_settings" {
    for_each = var.tenant_github_webhook_enabled ? [true] : []
    content {
      route_key              = "POST /v1/integrations/github/apps/{integration_id}/webhook"
      throttling_rate_limit  = var.tenant_github_webhook_throttle_rate
      throttling_burst_limit = var.tenant_github_webhook_throttle_burst
    }
  }

  dynamic "route_settings" {
    for_each = length(var.resource_plane_health_report_secret_arns) > 0 ? [true] : []
    content {
      route_key              = "POST /v1/internal/resource-planes/{resource_plane_id}/health"
      throttling_rate_limit  = var.resource_plane_health_report_throttle_rate
      throttling_burst_limit = var.resource_plane_health_report_throttle_burst
    }
  }

  dynamic "route_settings" {
    for_each = length(var.resource_plane_workload_spec_secret_arns) > 0 ? [true] : []
    content {
      route_key              = "GET /v1/internal/resource-planes/{resource_plane_id}/tenants/{tenant_id}/workloads/{workload_run_id}/attempts/{attempt_id}/generations/{generation}/specification"
      throttling_rate_limit  = var.resource_plane_health_report_throttle_rate
      throttling_burst_limit = var.resource_plane_health_report_throttle_burst
    }
  }
}

resource "aws_lambda_permission" "api_gateway" {
  statement_id  = "AllowHakoHTTPAPIInvoke"
  action        = "lambda:InvokeFunction"
  function_name = local.api_lambda_image_active ? aws_lambda_function.api_image[0].function_name : aws_lambda_function.api.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.control_plane.execution_arn}/*/*"
}

resource "aws_cognito_resource_server" "hako_api" {
  identifier   = "hako"
  name         = "Hako API"
  user_pool_id = var.cognito_user_pool_id

  scope {
    scope_name        = "api"
    scope_description = "Access Hako API endpoints."
  }
}

resource "aws_cognito_user_pool_client" "cli" {
  name         = var.cognito_cli_client_name
  user_pool_id = var.cognito_user_pool_id

  generate_secret                      = false
  enable_token_revocation              = true
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes = [
    "openid",
    "email",
    "profile",
    aws_cognito_resource_server.hako_api.scope_identifiers[0],
  ]
  callback_urls = var.cognito_cli_callback_urls
  supported_identity_providers = [
    "COGNITO",
  ]
}
