resource "aws_iam_role_policy" "tenant_private_key_readers" {
  count = length(var.tenant_private_key_reader_role_arns) == 0 ? 0 : 1

  name = "${var.policy_name_prefix}-tenant-private-key-readers"
  role = var.broker_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "AssumeApprovedTenantPrivateKeyReaders"
      Effect   = "Allow"
      Action   = ["sts:AssumeRole"]
      Resource = sort(tolist(var.tenant_private_key_reader_role_arns))
    }]
  })
}
