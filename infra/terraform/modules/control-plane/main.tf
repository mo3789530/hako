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
    Statement = [
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
    ]
  })
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
      HAKO_COGNITO_ISSUER                  = var.cognito_issuer_url
      HAKO_COGNITO_CLIENT_ID               = aws_cognito_user_pool_client.cli.id
      HAKO_DSQL_HOST                       = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                       = "hako_api"
      HAKO_DSQL_DATABASE                   = "postgres"
      HAKO_DEFAULT_WORKSPACE_IMAGE         = var.default_workspace_image
      HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS = var.default_workspace_runtime_class
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
      HAKO_COGNITO_ISSUER                  = var.cognito_issuer_url
      HAKO_COGNITO_CLIENT_ID               = aws_cognito_user_pool_client.cli.id
      HAKO_DSQL_HOST                       = "${aws_dsql_cluster.control_plane.identifier}.dsql.${var.aws_region}.on.aws"
      HAKO_DSQL_USER                       = "hako_api"
      HAKO_DSQL_DATABASE                   = "postgres"
      HAKO_DEFAULT_WORKSPACE_IMAGE         = var.default_workspace_image
      HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS = var.default_workspace_runtime_class
      HAKO_API_HTTP_MODE                   = "true"
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

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.control_plane.id
  name        = "$default"
  auto_deploy = true
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
