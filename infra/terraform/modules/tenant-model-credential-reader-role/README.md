# Tenant model credential reader Role

Creates one target-account IAM Role for one Hako Tenant model credential. Its
trust policy names only the exact Hako GitHub Credential Broker Role ARN. Its
identity policy grants `secretsmanager:GetSecretValue` on one exact Secret ARN
and optionally `kms:Decrypt` on one exact customer-managed KMS key, conditioned
on the Secrets Manager service path and that Secret's encryption context.

The Control Plane broker allowlist remains a separate explicit step: pass this
module's `role_arn` output to
`tenant_model_credential_reader_role_arns`. Configure the actual Secret's
resource policy separately when required for cross-account access. The Broker
also supplies an STS session policy restricted to the pinned Secret ARN and
version. This module does not create or read Secrets, write secret values,
create cross-account Secret resource policies, or grant access to GitHub App
credentials. Its plan tests use only synthetic ARNs with a mocked AWS provider.

Set the module provider to the account and Region that own the Secret. The
module rejects Secret or customer-managed KMS ARNs outside that account and
Region; it also rejects wildcard broker and Secret ARNs.

For customer-managed keys, the policy uses Secrets Manager's documented
`kms:ViaService` and `kms:EncryptionContext:SecretARN` conditions. See the
[AWS Secrets Manager encryption documentation](https://docs.aws.amazon.com/secretsmanager/latest/userguide/security-encryption.html).
