output "role_arn" {
  description = "Exact Tenant model credential reader Role ARN to add to the Control Plane Broker allowlist."
  value       = aws_iam_role.tenant_model_credential_reader.arn
}

output "role_name" {
  description = "Name of the dedicated Tenant model credential reader Role."
  value       = aws_iam_role.tenant_model_credential_reader.name
}
