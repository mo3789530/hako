# Task 56: Tenant GitHub App Registry and Configuration API

Status: Ready. Priority: P0. Depends on: Task 37 (complete).

## Goal

Implement the metadata/API foundation in [ADR 0007](../docs/adr/0007-tenant-github-apps.md) and [Tenant App design](../docs/tenant-github-apps.md). Do not enable Tenant-owned token issuance or change existing shared bindings.

## Work

- Add app registration, Tenant integration, default-selection revision, and platform-approved credential-binding metadata. Store references only, never Secret values.
- Enforce global App ID uniqueness, owned App single-Tenant ownership, immutable App ID, and one active default per Tenant using DB constraints/OCC. Preserve global Installation uniqueness.
- Implement sanitized list/create/detail/verify-request/select/disable API contracts and matching CLI metadata commands. Until verifier/setup are available, owned configurations remain inert draft; selection/activation must reject them.
- Require Owner/Admin; use hidden 404 for other Tenant/member access, idempotency for creation and expected revision for changes. Reject raw keys, arbitrary ARN/Role/URL, and unapproved credential binding before Secret access.
- Define an operator-only approved-binding registration interface/procedure; do not expose provisioning powers to Tenant users.

## Acceptance / tests / docs

- PostgreSQL/API/CLI tests cover two Tenants, role matrix, duplicate App ownership, cross-Tenant binding references, concurrent selection, idempotency and safe responses/audit.
- Shared legacy setup/Run creation behavior is unchanged. Owned feature stays default-off; no Secret read, IAM expansion, or AWS apply.
- DSQL/Postgres-compatible migration and updated API/error documentation are included. Migration of existing binding/Run identity is Task 59.
