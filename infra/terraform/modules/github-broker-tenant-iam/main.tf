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

resource "aws_iam_role_policy" "tenant_model_credential_readers" {
  count = length(var.tenant_model_credential_reader_role_arns) == 0 ? 0 : 1

  name = "${var.policy_name_prefix}-tenant-model-credential-readers"
  role = var.broker_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "AssumeApprovedTenantModelCredentialReaders"
      Effect   = "Allow"
      Action   = ["sts:AssumeRole"]
      Resource = sort(tolist(var.tenant_model_credential_reader_role_arns))
    }]
  })
}

resource "aws_iam_role_policy" "agent_proposal_artifact_readers" {
  count = length(var.agent_proposal_artifact_object_arns) == 0 && length(var.agent_proposal_artifact_kms_key_arns) == 0 ? 0 : 1

  name = "${var.policy_name_prefix}-agent-proposal-artifact-readers"
  role = var.broker_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      length(var.agent_proposal_artifact_object_arns) == 0 ? [] : [{
        Sid      = "ReadAgentProposalArchivesOnly"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = sort(tolist(var.agent_proposal_artifact_object_arns))
      }],
      length(var.agent_proposal_artifact_kms_key_arns) == 0 ? [] : [{
        Sid      = "DecryptAgentProposalArtifactsOnly"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = sort(tolist(var.agent_proposal_artifact_kms_key_arns))
      }]
    )
  })
}
