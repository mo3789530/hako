# Task 59: Tenant App Compatibility Migration and Credential IAM

残作業の実行単位：[Task 69](task-69-tenant-checkout-broker.md)、[Task 70](task-70-tenant-migration-cutover.md)。親タスクは集約用に保持し、子タスク完了だけで配備済みとはしない。

Status: Blocked on approved cross-account provisioning and production cutover. Tenant App-to-Installation binding, immutable Run pinning, Task 70 migration plan/backfill/drain guards, Resource Plane Worker → mTLS checkout broker composition, and exact broker-only `sts:AssumeRole` Terraform allowlisting are implemented and locally verified. Remaining work requires target Role trust/Secret/KMS provisioning, production migration and drain/cutover, deployment review, and operational rollback verification. No AWS apply or production GitHub change was made. Priority: P0.
External acceptance blocked: cross-account trust/Secret/KMS setup and production migration/cutover require the designated AWS environment and operational review; no AWS or production GitHub changes were made in this local run.

## Goal

Prepare safe shared-to-owned rollout and exact Secret-purpose IAM using [Tenant App design sections 4/8](../docs/tenant-github-apps.md). Terraform apply and Tenant cutover require separate approval.

## Work

- Backfill shared registration/integrations and bind Installation/Repository/Inbox/Run identity; validate missing/ambiguous metadata before adding final constraints. Preserve global Installation uniqueness and existing repository ownership policy.
- Expire old OAuth states and pin/drain existing Runs. Add versioned dispatch identity and worker capability gates so old workers cannot process owned Runs.
- Implement server-resolved integration identity in Run/checkout interfaces. Prevent switching defaults from mutating existing Run credentials. Coordinate with Tasks 38/39 as needed without enabling execution early.
- Define approved per-Tenant/purpose reader roles with exact Secret ARN and scoped KMS permissions, broker-only trust, operator-only mutation and no RP/API key read. Provide dry-run migration and secret-free Terraform validation/plan tests.
- Document drain/cancel/cleanup, explicit default switch, repository collision resolution, non-destructive rollback, and approved operator provisioning.

## Acceptance / tests / docs

Progress: Added the Tenant App/Installation association relation and Workload Run credential identity columns. New Run creation resolves the active App from persisted Tenant + Installation state and stores the integration ID, config revision and credential generation; an active Tenant-owned App with an unbound Installation fails closed instead of using shared credentials. Run JSON carries the immutable pin through dispatch. `repositorycheckout` requires a run-aware credential provider for owned Runs and refuses the legacy shared provider; `githubapp.TokenClient.CheckoutRunToken` checks the persisted pin against its resolver-derived client identity. The Resource Plane Lambda Worker composes this brokered checkout path with immutable Control Plane execution metadata and source staging; missing Tenant pins fail closed without shared fallback. Existing Runs with null identity remain explicitly shared/legacy and are not rewritten. Local PostgreSQL, mTLS and fake-provider tests cover pinning and no-fallback. Terraform grants `sts:AssumeRole` only to exact Tenant private-key reader ARNs on the existing broker role; it does not grant these roles to the API Lambda.

- Remaining: production migration execution/interrupted recovery, queued Run drain/cancel operation, cross-account reader Role trust policies and exact Secret/KMS permissions, broker certificate/private-key deployment, and rollback/operator procedure verification. Local Installation backfill and legacy Run guard are implemented in Task 70.

- Old-schema fixtures, interrupted backfill, wrong App/Tenant/Installation, queued Run pinning, shared compatibility and rollback are covered in isolated PostgreSQL tests.
- Terraform policy tests reject wildcard/wrong-purpose reader ARNs and verify that only the broker role can assume the exact private-key reader set. Cross-account target Role trust and exact Secret/KMS policies still require operator provisioning/review. No AWS apply or actual Secret values in Terraform state.
- Both feature flag and workload creation gate stay default-off until their respective release criteria pass.
