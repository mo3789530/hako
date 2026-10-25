# Task 56: Tenant GitHub App Registry and Configuration API

Status: Done (metadata/API/CLI, docs, and PostgreSQL integration validation; 2026-10-10). Priority: P0. Depends on: Task 37 (complete).

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

## Progress (2026-10-10)

- Added additive metadata tables, API/CLI routes, default-off management flag, API-role read-only binding access, and operator-only SQL registration guidance.
- Owned configurations remain inert drafts; verify/default selection return `github_app_not_ready` until later tasks provide verification and activation.
- Unit and PostgreSQL integration tests passed, including Tenant isolation, role checks, duplicate App ownership, idempotency, audit safety, migration application, and concurrent default-row uniqueness. Tests use an isolated schema on the existing local PostgreSQL container; the database remains running after verification.

## CLI/API wiring refresh (2026-10-24)

- Added a local HTTP-backed CLI test for Tenant App list/add/disable/select-default,
  setup, and failed-delivery listing. It verifies Tenant-scoped endpoint paths,
  Bearer auth, pagination, expected revision/generation payloads, add
  idempotency, and that App creation sends an approved credential-binding ID
  without raw Secret/private-key fields. Setup uses a stubbed browser opener;
  no real GitHub or AWS calls occur. The same `cmd/hako` suite exercises the
  existing Repository configuration CLI paths; aggregate statement coverage is
  now 46.9%.

## Verification configuration response (2026-10-25)

The API's internal error now describes the actual missing dependency (trusted
proof verifier unavailable), rather than claiming verification is not
implemented. A PostgreSQL API integration assertion confirms the Owner-facing
response remains `409 github_app_not_ready` when no proof broker is configured.

## Repository listing authorization coverage (2026-10-25)

The PostgreSQL API integration test now exercises the successful repository
listing route for an active, Tenant-bound Installation, including its stable
ordering and Tenant/Installation identity. It also verifies that a Tenant
Member and an Owner from another Tenant receive the hidden `404`, pending
Installations remain unavailable, and zero or malformed Installation IDs are
rejected with `400`. This uses local database fixtures only; it does not call
GitHub or AWS.
