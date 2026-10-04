# Task 59: Tenant App Compatibility Migration and Credential IAM

Status: Blocked on Tasks 56–58. Priority: P0.

## Goal

Prepare safe shared-to-owned rollout and exact Secret-purpose IAM using [Tenant App design sections 4/8](../docs/tenant-github-apps.md). Terraform apply and Tenant cutover require separate approval.

## Work

- Backfill shared registration/integrations and bind Installation/Repository/Inbox/Run identity; validate missing/ambiguous metadata before adding final constraints. Preserve global Installation uniqueness and existing repository ownership policy.
- Expire old OAuth states and pin/drain existing Runs. Add versioned dispatch identity and worker capability gates so old workers cannot process owned Runs.
- Implement server-resolved integration identity in Run/checkout interfaces. Prevent switching defaults from mutating existing Run credentials. Coordinate with Tasks 38/39 as needed without enabling execution early.
- Define approved per-Tenant/purpose reader roles with exact Secret ARN and scoped KMS permissions, broker-only trust, operator-only mutation and no RP/API key read. Provide dry-run migration and secret-free Terraform validation/plan tests.
- Document drain/cancel/cleanup, explicit default switch, repository collision resolution, non-destructive rollback, and approved operator provisioning.

## Acceptance / tests / docs

- Old-schema fixtures, interrupted backfill, wrong App/Tenant/Installation, queued Run pinning, shared compatibility and rollback are covered in isolated PostgreSQL tests.
- Policy tests reject wildcard Secret reads, wrong Tenant Role/Secret, tag/provisioning privilege escalation and API/RP private-key access. No AWS apply or actual Secret values in Terraform state.
- Both feature flag and workload creation gate stay default-off until their respective release criteria pass.
