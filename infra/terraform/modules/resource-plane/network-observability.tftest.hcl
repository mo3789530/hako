mock_provider "aws" {
  alias = "mock"

  mock_data "aws_iam_policy_document" {
    defaults = {
      json          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
      minified_json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { name = "ap-northeast-1" }
  }
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/hako-rp-test-vpc-flow-logs"
      id  = "hako-rp-test-vpc-flow-logs"
    }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = {
      arn = "arn:aws:logs:ap-northeast-1:123456789012:log-group:/hako/resource-plane/rp-test/vpc-flow-logs"
      id  = "/hako/resource-plane/rp-test/vpc-flow-logs"
    }
  }
  mock_resource "aws_flow_log" {
    defaults = { id = "fl-0123456789abcdef0" }
  }
}

run "resource_plane_flow_logs_are_disabled_by_default" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
  }

  assert {
    condition     = length(aws_flow_log.resource_plane_vpc) == 0 && output.resource_plane_vpc_flow_log_id == null
    error_message = "Flow Logs must remain disabled until a VPC and explicit retention policy are supplied."
  }
}

run "resource_plane_flow_logs_capture_all_traffic_with_scoped_writer" {
  command   = plan
  providers = { aws = aws.mock }

  variables {
    resource_plane_id                      = "rp-test"
    control_plane_dispatcher_role_arn      = "arn:aws:iam::123456789012:role/hako-cp-dispatcher"
    control_plane_result_consumer_role_arn = "arn:aws:iam::123456789012:role/hako-cp-result-consumer"
    resource_plane_network_observability = {
      vpc_id             = "vpc-0123456789abcdef0"
      log_retention_days = 30
    }
  }

  assert {
    condition = (aws_flow_log.resource_plane_vpc[0].vpc_id == "vpc-0123456789abcdef0" &&
      aws_flow_log.resource_plane_vpc[0].traffic_type == "ALL" &&
      aws_flow_log.resource_plane_vpc[0].max_aggregation_interval == 60 &&
    aws_cloudwatch_log_group.resource_plane_vpc_flow_logs[0].retention_in_days == 30)
    error_message = "Configured Resource Plane Flow Logs must capture accepted and rejected traffic with bounded aggregation and retention."
  }

  assert {
    condition = (jsondecode(aws_iam_role.resource_plane_vpc_flow_logs[0].assume_role_policy).Statement[0].Principal.Service == "vpc-flow-logs.amazonaws.com" &&
      jsondecode(aws_iam_role.resource_plane_vpc_flow_logs[0].assume_role_policy).Statement[0].Condition.StringEquals["aws:SourceAccount"] == "123456789012" &&
    jsondecode(aws_iam_role.resource_plane_vpc_flow_logs[0].assume_role_policy).Statement[0].Condition.ArnLike["aws:SourceArn"] == "arn:aws:ec2:ap-northeast-1:123456789012:vpc-flow-log/*")
    error_message = "The flow-log publishing role must trust only the VPC Flow Logs service in this account and Region."
  }

  assert {
    condition = (jsondecode(aws_iam_role_policy.resource_plane_vpc_flow_logs[0].policy).Statement[0].Action == ["logs:DescribeLogGroups"] &&
      jsondecode(aws_iam_role_policy.resource_plane_vpc_flow_logs[0].policy).Statement[1].Resource == "arn:aws:logs:ap-northeast-1:123456789012:log-group:/hako/resource-plane/rp-test/vpc-flow-logs" &&
    jsondecode(aws_iam_role_policy.resource_plane_vpc_flow_logs[0].policy).Statement[2].Resource == "arn:aws:logs:ap-northeast-1:123456789012:log-group:/hako/resource-plane/rp-test/vpc-flow-logs:log-stream:*")
    error_message = "Flow Log writes must be scoped to the dedicated Resource Plane log group."
  }
}
