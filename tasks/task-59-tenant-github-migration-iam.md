# Task 59: Tenant App Compatibility Migration and Credential IAM

残作業の実行単位：[Task 69](task-69-tenant-checkout-broker.md)、[Task 70](task-70-tenant-migration-cutover.md)。親タスクは集約用に保持し、子タスク完了だけで配備済みとはしない。

Status: In Progress (Tenant App-to-Installation binding and immutable Workload Run integration/revision/generation pinning implemented with no owned-to-shared fallback; Task 70 now provides transactional revision/generation-fenced Installation backfill and legacy Run drain/default-selection guards. Trusted checkout worker wiring, purpose-specific IAM/resources and policy validation remain). Priority: P0.

## Goal

Prepare safe shared-to-owned rollout and exact Secret-purpose IAM using [Tenant App design sections 4/8](../docs/tenant-github-apps.md). Terraform apply and Tenant cutover require separate approval.

## Work

- Backfill shared registration/integrations and bind Installation/Repository/Inbox/Run identity; validate missing/ambiguous metadata before adding final constraints. Preserve global Installation uniqueness and existing repository ownership policy.
- Expire old OAuth states and pin/drain existing Runs. Add versioned dispatch identity and worker capability gates so old workers cannot process owned Runs.
- Implement server-resolved integration identity in Run/checkout interfaces. Prevent switching defaults from mutating existing Run credentials. Coordinate with Tasks 38/39 as needed without enabling execution early.
- Define approved per-Tenant/purpose reader roles with exact Secret ARN and scoped KMS permissions, broker-only trust, operator-only mutation and no RP/API key read. Provide dry-run migration and secret-free Terraform validation/plan tests.
- Document drain/cancel/cleanup, explicit default switch, repository collision resolution, non-destructive rollback, and approved operator provisioning.

## Acceptance / tests / docs

Progress: Added the Tenant App/Installation association relation and Workload Run credential identity columns. New Run creation resolves the active App from persisted Tenant + Installation state and stores the integration ID, config revision and credential generation; an active Tenant-owned App with an unbound Installation fails closed instead of using shared credentials. Run JSON carries the immutable pin through dispatch. `repositorycheckout` requires a run-aware credential provider for owned Runs and refuses the legacy shared provider; `githubapp.TokenClient.CheckoutRunToken` checks the persisted pin against its resolver-derived client identity. Existing Runs with null identity remain explicitly shared/legacy and are not rewritten. Local PostgreSQL tests exercise pinning and no-fallback. This is not yet connected to a concrete AWS-backed checkout broker or deployable IAM policy.

- Remaining: production migration execution/interrupted recovery, queued Run drain/cancel operation, exact purpose-specific trust/permission policy tests and Terraform definition, broker-only API/private-key deployment, and rollback/operator procedure verification. Local Installation backfill and legacy Run guard are implemented in Task 70.

- Old-schema fixtures, interrupted backfill, wrong App/Tenant/Installation, queued Run pinning, shared compatibility and rollback are covered in isolated PostgreSQL tests.
- Policy tests reject wildcard Secret reads, wrong Tenant Role/Secret, tag/provisioning privilege escalation and API/RP private-key access. No AWS apply or actual Secret values in Terraform state.
- Both feature flag and workload creation gate stay default-off until their respective release criteria pass.
