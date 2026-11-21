locals {
  workspace_gateway_secret_arns = var.workspace_gateway_deployment == null ? [] : [
    var.workspace_gateway_deployment.secret_arns.tls_certificate,
    var.workspace_gateway_deployment.secret_arns.tls_private_key,
    var.workspace_gateway_deployment.secret_arns.session_public_keys,
    var.workspace_gateway_deployment.secret_arns.session_lookup_hmac,
    var.workspace_gateway_deployment.secret_arns.connection_lease_hmac,
    var.workspace_gateway_deployment.secret_arns.agent_ticket_private_key,
  ]
  workspace_gateway_image_parts = var.workspace_gateway_deployment == null ? [] : try(regex(
    "^([0-9]{12})\\.dkr\\.ecr\\.([a-z0-9-]+)\\.amazonaws\\.com/([A-Za-z0-9._/-]+)@sha256:[0-9a-f]{64}$",
    var.workspace_gateway_deployment.image_uri,
  ), [])
  workspace_gateway_ecr_repository_arn = var.workspace_gateway_deployment == null || length(local.workspace_gateway_image_parts) != 3 ? "" : "arn:${data.aws_partition.current.partition}:ecr:${local.workspace_gateway_image_parts[1]}:${local.workspace_gateway_image_parts[0]}:repository/${local.workspace_gateway_image_parts[2]}"
  workspace_gateway_journal_prefix     = var.workspace_gateway_deployment == null ? "" : trim(var.workspace_gateway_deployment.journal_prefix, "/")
  workspace_gateway_journal_key        = local.workspace_gateway_journal_prefix == "" ? "${var.resource_plane_id}/*" : "${local.workspace_gateway_journal_prefix}/${var.resource_plane_id}/*"
  workspace_gateway_artifact_prefix    = var.workspace_gateway_deployment == null ? "" : var.workspace_gateway_deployment.workload_artifact_prefix
  workspace_gateway_artifact_key       = local.workspace_gateway_artifact_prefix == "" ? "" : "${local.workspace_gateway_artifact_prefix}/*"
  workspace_gateway_alb_name           = "${substr(var.resource_plane_id, 0, 20)}-gw-${substr(sha1(var.resource_plane_id), 0, 8)}"
  workspace_gateway_target_group_name  = "${substr(var.resource_plane_id, 0, 19)}-gwt-${substr(sha1(var.resource_plane_id), 0, 8)}"
  workspace_gateway_secret_loader_command = var.workspace_gateway_deployment == null ? "" : join("\n", [
    "set -eu",
    "umask 077",
    "fetch_secret() { aws secretsmanager get-secret-value --region \"$AWS_REGION\" --secret-id \"$2\" --query SecretString --output text --no-cli-pager > \"$1\"; test -s \"$1\"; }",
    "fetch_secret /run/hako-secrets/tls.crt \"$TLS_CERTIFICATE_ARN\"",
    "fetch_secret /run/hako-secrets/tls.key \"$TLS_PRIVATE_KEY_ARN\"",
    "fetch_secret /run/hako-secrets/session-public-keys.json \"$SESSION_PUBLIC_KEYS_ARN\"",
    "fetch_secret /run/hako-secrets/session-lookup-hmac \"$SESSION_LOOKUP_HMAC_ARN\"",
    "fetch_secret /run/hako-secrets/connection-lease-hmac \"$CONNECTION_LEASE_HMAC_ARN\"",
    "fetch_secret /run/hako-secrets/agent-ticket-private-key.pem \"$AGENT_TICKET_PRIVATE_KEY_ARN\"",
    "chown 65532:65532 /run/hako-secrets/*",
    "chmod 0400 /run/hako-secrets/*",
  ])
}

resource "aws_security_group" "workspace_gateway_alb" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name_prefix = "${var.resource_plane_id}-gateway-alb-"
  description = "HTTPS ingress for the Hako Workspace Gateway ALB."
  vpc_id      = var.workspace_gateway_deployment.vpc_id

  ingress {
    description = "Developer HTTPS and WSS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.workspace_gateway_deployment.ingress_cidr_blocks
  }

  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway-alb" })
}

resource "aws_security_group" "workspace_gateway_task" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name_prefix = "${var.resource_plane_id}-gateway-task-"
  description = "Private Hako Workspace Gateway tasks."
  vpc_id      = var.workspace_gateway_deployment.vpc_id

  egress {
    description = "DNS to the VPC resolver"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = [var.workspace_gateway_deployment.vpc_cidr_block]
  }

  egress {
    description = "DNS over TCP to the VPC resolver"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = [var.workspace_gateway_deployment.vpc_cidr_block]
  }

  dynamic "egress" {
    for_each = var.workspace_gateway_deployment.egress_cidr_blocks
    content {
      description = "HTTPS to reviewed Control Plane, AWS API, and MicroVM endpoints"
      from_port   = 443
      to_port     = 443
      protocol    = "tcp"
      cidr_blocks = [egress.value]
    }
  }

  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway-task" })
}

resource "aws_vpc_security_group_ingress_rule" "workspace_gateway_from_alb" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  security_group_id            = aws_security_group.workspace_gateway_task[0].id
  referenced_security_group_id = aws_security_group.workspace_gateway_alb[0].id
  description                  = "TLS health checks and WSS from the ALB"
  ip_protocol                  = "tcp"
  from_port                    = 8443
  to_port                      = 8443
}

resource "aws_vpc_security_group_egress_rule" "workspace_gateway_to_task" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  security_group_id            = aws_security_group.workspace_gateway_alb[0].id
  referenced_security_group_id = aws_security_group.workspace_gateway_task[0].id
  description                  = "TLS to the private Gateway tasks"
  ip_protocol                  = "tcp"
  from_port                    = 8443
  to_port                      = 8443
}

resource "aws_lb" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name                       = local.workspace_gateway_alb_name
  internal                   = false
  load_balancer_type         = "application"
  security_groups            = [aws_security_group.workspace_gateway_alb[0].id]
  subnets                    = var.workspace_gateway_deployment.public_subnet_ids
  enable_deletion_protection = var.workspace_gateway_deployment.deletion_protection
  drop_invalid_header_fields = true
  idle_timeout               = 3600

  dynamic "access_logs" {
    for_each = var.workspace_gateway_deployment.access_log_bucket == "" ? [] : [1]
    content {
      bucket  = var.workspace_gateway_deployment.access_log_bucket
      prefix  = "hako/${var.resource_plane_id}/workspace-gateway"
      enabled = true
    }
  }

  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_lb_target_group" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name                 = local.workspace_gateway_target_group_name
  port                 = 8443
  protocol             = "HTTPS"
  protocol_version     = "HTTP1"
  target_type          = "ip"
  vpc_id               = var.workspace_gateway_deployment.vpc_id
  deregistration_delay = 30

  health_check {
    enabled             = true
    protocol            = "HTTPS"
    path                = "/healthz"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_lb_listener" "workspace_gateway_https" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  load_balancer_arn = aws_lb.workspace_gateway[0].arn
  port              = 443
  protocol          = "HTTPS"
  certificate_arn   = var.workspace_gateway_deployment.acm_certificate_arn
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.workspace_gateway[0].arn
  }
}

resource "aws_iam_role" "workspace_gateway_execution" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name_prefix = "${var.resource_plane_id}-gateway-exec-"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway-execution" })
}

resource "aws_iam_role_policy" "workspace_gateway_execution" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-gateway-execution"
  role = aws_iam_role.workspace_gateway_execution[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "AuthenticateECRPull"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid      = "PullOnlyGatewayImageRepository"
        Effect   = "Allow"
        Action   = ["ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage"]
        Resource = local.workspace_gateway_ecr_repository_arn
      },
      {
        Sid      = "WriteOnlyGatewayTaskLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.workspace_gateway[0].arn}:*"
      }
    ]
  })
}

resource "aws_iam_role" "workspace_gateway_task" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name_prefix = "${var.resource_plane_id}-gateway-"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_iam_role_policy" "workspace_gateway_task" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name = "${var.resource_plane_id}-gateway-runtime"
  role = aws_iam_role.workspace_gateway_task[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ReadOnlyThisResourcePlaneWorkspaceJournals"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.workspace_gateway_deployment.journal_bucket_arn}/${local.workspace_gateway_journal_key}"
      },
      {
        Sid      = "InspectOnlyConfiguredWorkspaceMicroVMImage"
        Effect   = "Allow"
        Action   = ["lambda:GetMicrovm", "lambda:CreateMicrovmAuthToken"]
        Resource = var.workspace_gateway_deployment.microvm_image_arn
      },
      {
        Sid      = "ReadOnlyConfiguredGatewayBootstrapSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = local.workspace_gateway_secret_arns
      }
      ], length(var.workspace_gateway_deployment.kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptOnlyConfiguredGatewaySecrets"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.workspace_gateway_deployment.kms_key_arns
        Condition = {
          StringEquals             = { "kms:ViaService" = "secretsmanager.${data.aws_region.current.name}.${data.aws_partition.current.dns_suffix}" }
          "ForAnyValue:StringLike" = { "kms:EncryptionContext:SecretARN" = local.workspace_gateway_secret_arns }
        }
        }], var.workspace_gateway_deployment.workload_artifact_bucket_name == "" ? [] : [{
        Sid      = "ReadOnlyConfiguredWorkloadArtifactPrefix"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.workspace_gateway_deployment.workload_artifact_bucket_arn}/${local.workspace_gateway_artifact_key}"
    }])
  })
}

check "workspace_gateway_image_account_region" {
  assert {
    condition = var.workspace_gateway_deployment == null || (
      length(local.workspace_gateway_image_parts) == 3 &&
      local.workspace_gateway_image_parts[0] == data.aws_caller_identity.current.account_id &&
      local.workspace_gateway_image_parts[1] == data.aws_region.current.name
    )
    error_message = "The pinned Workspace Gateway ECR image must be in this Resource Plane AWS account and Region."
  }
}

resource "aws_cloudwatch_log_group" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name              = "/hako/${var.resource_plane_id}/workspace-gateway"
  retention_in_days = var.workspace_gateway_deployment.log_retention_days
  tags              = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_ecs_task_definition" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  family                   = "${var.resource_plane_id}-workspace-gateway"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.workspace_gateway_deployment.task_cpu)
  memory                   = tostring(var.workspace_gateway_deployment.task_memory)
  execution_role_arn       = aws_iam_role.workspace_gateway_execution[0].arn
  task_role_arn            = aws_iam_role.workspace_gateway_task[0].arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  volume {
    name = "hako-gateway-secrets"
  }

  container_definitions = jsonencode([
    {
      name       = "secret-loader"
      image      = var.workspace_gateway_deployment.secret_loader_image_uri
      essential  = false
      user       = "0"
      entryPoint = ["/bin/sh", "-c"]
      command    = [local.workspace_gateway_secret_loader_command]
      environment = [
        { name = "AWS_REGION", value = data.aws_region.current.name },
        { name = "TLS_CERTIFICATE_ARN", value = var.workspace_gateway_deployment.secret_arns.tls_certificate },
        { name = "TLS_PRIVATE_KEY_ARN", value = var.workspace_gateway_deployment.secret_arns.tls_private_key },
        { name = "SESSION_PUBLIC_KEYS_ARN", value = var.workspace_gateway_deployment.secret_arns.session_public_keys },
        { name = "SESSION_LOOKUP_HMAC_ARN", value = var.workspace_gateway_deployment.secret_arns.session_lookup_hmac },
        { name = "CONNECTION_LEASE_HMAC_ARN", value = var.workspace_gateway_deployment.secret_arns.connection_lease_hmac },
        { name = "AGENT_TICKET_PRIVATE_KEY_ARN", value = var.workspace_gateway_deployment.secret_arns.agent_ticket_private_key }
      ]
      mountPoints = [{ sourceVolume = "hako-gateway-secrets", containerPath = "/run/hako-secrets", readOnly = false }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.workspace_gateway[0].name
          "awslogs-region"        = data.aws_region.current.name
          "awslogs-stream-prefix" = "secret-loader"
        }
      }
    },
    {
      name                   = "workspace-gateway"
      image                  = var.workspace_gateway_deployment.image_uri
      essential              = true
      user                   = "65532:65532"
      readonlyRootFilesystem = true
      portMappings           = [{ containerPort = 8443, hostPort = 8443, protocol = "tcp" }]
      dependsOn              = [{ containerName = "secret-loader", condition = "SUCCESS" }]
      environment = [
        { name = "HAKO_RESOURCE_PLANE_ID", value = var.resource_plane_id },
        { name = "HAKO_GATEWAY_LISTEN_ADDRESS", value = ":8443" },
        { name = "HAKO_GATEWAY_TLS_CERT_FILE", value = "/run/hako-secrets/tls.crt" },
        { name = "HAKO_GATEWAY_TLS_KEY_FILE", value = "/run/hako-secrets/tls.key" },
        { name = "HAKO_GATEWAY_SESSION_PUBLIC_KEYS_FILE", value = "/run/hako-secrets/session-public-keys.json" },
        { name = "HAKO_GATEWAY_SESSION_LOOKUP_ENDPOINT", value = var.workspace_gateway_deployment.session_lookup_endpoint },
        { name = "HAKO_GATEWAY_SESSION_LOOKUP_KEY_FILE", value = "/run/hako-secrets/session-lookup-hmac" },
        { name = "HAKO_GATEWAY_CONNECTION_LEASE_ENDPOINT", value = var.workspace_gateway_deployment.connection_lease_endpoint },
        { name = "HAKO_GATEWAY_CONNECTION_LEASE_KEY_FILE", value = "/run/hako-secrets/connection-lease-hmac" },
        { name = "HAKO_GATEWAY_AGENT_TICKET_KEY_ID", value = var.workspace_gateway_deployment.agent_ticket_key_id },
        { name = "HAKO_GATEWAY_AGENT_TICKET_KEY_FILE", value = "/run/hako-secrets/agent-ticket-private-key.pem" },
        { name = "HAKO_GATEWAY_PUBLIC_ORIGIN", value = var.workspace_gateway_deployment.public_origin },
        { name = "HAKO_GATEWAY_ALLOWED_ORIGINS", value = join(",", var.workspace_gateway_deployment.allowed_origins) },
        { name = "HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET", value = var.workspace_gateway_deployment.journal_bucket_name },
        { name = "HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX", value = local.workspace_gateway_journal_prefix },
        { name = "HAKO_WORKLOAD_ARTIFACT_BUCKET", value = var.workspace_gateway_deployment.workload_artifact_bucket_name },
        { name = "HAKO_WORKLOAD_ARTIFACT_PREFIX", value = local.workspace_gateway_artifact_prefix },
        { name = "HAKO_GATEWAY_MAX_CONNECTIONS", value = tostring(var.workspace_gateway_deployment.max_connections) },
        { name = "HAKO_GATEWAY_MAX_CONNECTIONS_PER_WORKSPACE", value = tostring(var.workspace_gateway_deployment.max_connections_per_workspace) },
        { name = "HAKO_GATEWAY_SESSION_RECHECK_INTERVAL", value = "15s" },
        { name = "AWS_REGION", value = data.aws_region.current.name }
      ]
      mountPoints = [{ sourceVolume = "hako-gateway-secrets", containerPath = "/run/hako-secrets", readOnly = true }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.workspace_gateway[0].name
          "awslogs-region"        = data.aws_region.current.name
          "awslogs-stream-prefix" = "gateway"
        }
      }
    }
  ])
  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_ecs_service" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name                               = "${var.resource_plane_id}-workspace-gateway"
  cluster                            = var.workspace_gateway_deployment.ecs_cluster_arn
  task_definition                    = aws_ecs_task_definition.workspace_gateway[0].arn
  desired_count                      = var.workspace_gateway_deployment.desired_count
  launch_type                        = "FARGATE"
  platform_version                   = "LATEST"
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  enable_execute_command             = false
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.workspace_gateway_deployment.private_subnet_ids
    security_groups  = [aws_security_group.workspace_gateway_task[0].id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.workspace_gateway[0].arn
    container_name   = "workspace-gateway"
    container_port   = 8443
  }

  depends_on = [aws_lb_listener.workspace_gateway_https]
  tags       = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_cloudwatch_metric_alarm" "workspace_gateway_unhealthy_targets" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-gateway-unhealthy-targets"
  alarm_description   = "Workspace Gateway ALB has an unhealthy Fargate target."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    LoadBalancer = aws_lb.workspace_gateway[0].arn_suffix
    TargetGroup  = aws_lb_target_group.workspace_gateway[0].arn_suffix
  }
  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_cloudwatch_metric_alarm" "workspace_gateway_alb_5xx" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  alarm_name          = "${var.resource_plane_id}-gateway-alb-5xx"
  alarm_description   = "Workspace Gateway ALB returned repeated 5xx responses."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_ELB_5XX_Count"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 5
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    LoadBalancer = aws_lb.workspace_gateway[0].arn_suffix
  }
  tags = merge(local.common_tags, { "hako:component" = "workspace-gateway" })
}

resource "aws_appautoscaling_target" "workspace_gateway" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  max_capacity       = var.workspace_gateway_deployment.max_capacity
  min_capacity       = var.workspace_gateway_deployment.min_capacity
  resource_id        = "service/${var.workspace_gateway_deployment.ecs_cluster_name}/${aws_ecs_service.workspace_gateway[0].name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

resource "aws_appautoscaling_policy" "workspace_gateway_cpu" {
  count = var.workspace_gateway_deployment == null ? 0 : 1

  name               = "${var.resource_plane_id}-gateway-cpu"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.workspace_gateway[0].resource_id
  scalable_dimension = aws_appautoscaling_target.workspace_gateway[0].scalable_dimension
  service_namespace  = aws_appautoscaling_target.workspace_gateway[0].service_namespace
  target_tracking_scaling_policy_configuration {
    target_value       = 60
    scale_in_cooldown  = 180
    scale_out_cooldown = 60
    predefined_metric_specification { predefined_metric_type = "ECSServiceAverageCPUUtilization" }
  }
}
