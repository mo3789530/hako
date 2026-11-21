locals {
  network_flow_logs_group_name = "/hako/resource-plane/${var.resource_plane_id}/vpc-flow-logs"
  network_flow_logs_group_arn  = "arn:${data.aws_partition.current.partition}:logs:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:log-group:${local.network_flow_logs_group_name}"
}

resource "aws_cloudwatch_log_group" "resource_plane_vpc_flow_logs" {
  count = var.resource_plane_network_observability == null ? 0 : 1

  name              = local.network_flow_logs_group_name
  retention_in_days = var.resource_plane_network_observability.log_retention_days
  kms_key_id        = var.resource_plane_network_observability.kms_key_arn != "" ? var.resource_plane_network_observability.kms_key_arn : null

  tags = merge(local.common_tags, { "hako:log-purpose" = "vpc-flow-logs" })
}

resource "aws_iam_role" "resource_plane_vpc_flow_logs" {
  count = var.resource_plane_network_observability == null ? 0 : 1

  name = "hako-${var.resource_plane_id}-vpc-flow-logs"
  path = "/hako/resource-planes/${var.resource_plane_id}/"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "vpc-flow-logs.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
        ArnLike = {
          "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:vpc-flow-log/*"
        }
      }
    }]
  })

  tags = merge(local.common_tags, { "hako:role-purpose" = "vpc-flow-logs" })
}

resource "aws_iam_role_policy" "resource_plane_vpc_flow_logs" {
  count = var.resource_plane_network_observability == null ? 0 : 1

  name = "publish-to-vpc-flow-logs"
  role = aws_iam_role.resource_plane_vpc_flow_logs[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "DescribeLogGroups"
        Effect   = "Allow"
        Action   = ["logs:DescribeLogGroups"]
        Resource = "*"
      },
      {
        Sid      = "EnsureDedicatedFlowLogGroupExists"
        Effect   = "Allow"
        Action   = ["logs:CreateLogGroup"]
        Resource = local.network_flow_logs_group_arn
      },
      {
        Sid    = "WriteOnlyToResourcePlaneFlowLogStreams"
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:DescribeLogStreams",
          "logs:PutLogEvents"
        ]
        Resource = "${local.network_flow_logs_group_arn}:log-stream:*"
      }
    ]
  })
}

resource "aws_flow_log" "resource_plane_vpc" {
  count = var.resource_plane_network_observability == null ? 0 : 1

  vpc_id                   = var.resource_plane_network_observability.vpc_id
  traffic_type             = "ALL"
  log_destination_type     = "cloud-watch-logs"
  log_destination          = aws_cloudwatch_log_group.resource_plane_vpc_flow_logs[0].arn
  iam_role_arn             = aws_iam_role.resource_plane_vpc_flow_logs[0].arn
  max_aggregation_interval = 60
  log_format               = "$${version} $${account-id} $${interface-id} $${srcaddr} $${dstaddr} $${srcport} $${dstport} $${protocol} $${packets} $${bytes} $${start} $${end} $${action} $${log-status}"

  tags = merge(local.common_tags, { "hako:resource-purpose" = "vpc-flow-logs" })
}
