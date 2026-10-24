# Task 54: Preview Workspaces, Software Catalog, and OIDC Deploy Policy

Status: In Progress. The Tenant-scoped Software Catalog metadata API/CLI and
PostgreSQL RBAC/reference integration coverage are implemented. OIDC deploy
policy API/CLI and fixed-GitHub-issuer RS256/JWKS claim verification are now
implemented locally; no STS exchange or deployment execution is enabled.
Tenant-scoped Preview opt-in and transactional PR desired-state tracking are
implemented locally, including stale GitHub `updated_at` fencing. Preview
MicroVM allocation, Gateway routing, expiry and cleanup execution/retries, and
production IAM wiring remain incomplete. The dormant approval verifier checks
the live Actions run attempt against signed repository, owner, ref, SHA,
workflow and event claims, checks pending deployments and approval history,
and requires Environment reviewers with self-review disabled. Priority: P3.

## Goal

Add optional repository-level preview environments and service metadata/deploy policy after the Workspace Runtime and Gateway are production-ready.

## Work

- Create one isolated Preview Run/Workspace per authorized PR commit; expose only through Gateway with expiring Tenant-scoped routing; tear down on PR close/merge/timeout.
- Add a Tenant-scoped Software Catalog mapping service, owner/team, repository, runtime/language, dependencies, and production URL without duplicating secret data.
- Add GitHub OIDC cloud identity only for trusted release/deploy Workloads; constrain cloud trust by Repository, ref/environment, and verified workflow claims. Local dormant policy verification is implemented; STS exchange and cloud identity are not.
- Require explicit human approval and policy checks for production; untrusted forks and agents receive no deploy role. Local verification checks the exact live run and approval state but does not authorize deployment.

## Software Catalog metadata slice (2026-10-24)

- Added Tenant-scoped `GET /v1/tenants/{tenant_id}/software-catalog` and
  `PUT /v1/tenants/{tenant_id}/software-catalog/{service_id}`. Tenant members
  may read; only Owners/Admins may write. Writes require a Repository already
  registered to that same Tenant and dependencies already present in that
  Tenant. Catalog mutation and audit append share one transaction.
- Added `hako catalog list` and `hako catalog set`. Input is bounded strict
  JSON; it supports only the `lambda-microvm` runtime, validated language and
  service identifiers, bounded sorted dependencies, and optional HTTPS
  production URL metadata. No credentials or Secret references are accepted.
- Added DSQL migration 000138 and explicit `hako_api` table grants. Local
  unit/client/CLI and PostgreSQL integration tests cover RBAC, cross-Tenant
  hiding, Repository ownership, dependency validation, EC2 rejection, and
  unknown secret fields. See [`Software Catalog`](../docs/software-catalog.md).
- Added migration 000139 and Tenant RBAC APIs/CLI for a dormant OIDC policy.
  The allowlist requires an exact AWS STS audience, refs, environments and
  workflow refs. `POST .../deploy-policy/verify` validates the GitHub issuer's
  RS256 signature and exact Repository/Owner ID, ref/environment/workflow/event
  claims, including legacy and immutable-ID `sub` formats. Tokens are not
  stored; no AWS credentials are exchanged and every response keeps
  `execution_enabled=false`. Verification now fails closed unless a
  Tenant/Installation/Repository-scoped GitHub Environment approval verifier
  confirms an exact Environment `required_reviewers` rule. A reusable GitHub
  App client verifies a non-empty User/Team reviewer set with a short-lived
  `actions:read` token. The production API calls the Control Plane-only
  mTLS GitHub Credential Broker, which resolves Tenant App credentials and
  rechecks the policy pin after GitHub I/O. The broker receives read-only
  Software Catalog DSQL access. Unit/API integration coverage checks configured
  approval verification and missing-verifier failure; broker service and
  transport tests cover caller, pin, and denial behavior. Configured protection
  is not proof that a specific run was approved.
- Preview allocation/routing and PR lifecycle cleanup remain blocked by the
  listed runtime/deployment prerequisites. Production approval validation,
  IAM trust/exchange and deployment execution remain disabled.

## Preview lifecycle metadata slice (2026-10-24)

- Added default-off Tenant Repository Preview opt-in and Owner/Admin API at
  `PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/preview`.
- The durable inbox processor now applies PR opened/reopened/synchronize/closed
  events in the same transaction as delivery acknowledgement. State is keyed
  by Tenant, Installation, Repository, and PR; `updated_at` fences delayed old
  GitHub events, while duplicate Delivery IDs remain idempotent.
- Open PRs become `awaiting_runtime`; closed PRs become `cleanup_pending`.
  Migration 000140 adds the opt-in, and 000141 adds the lifecycle table.
  Runtime allocation is explicitly reported disabled; this change does not
  create a MicroVM, Workspace, or Gateway route.
- `go test ./...` passed. PostgreSQL Preview lifecycle E2E passed against the
  local `hako-dev-postgres`, including stale delivery, close/reopen, Tenant
  scoping, and duplicate replay.
- Remaining: Preview Workspace/Run binding to Lambda MicroVM, expiring
  Tenant-scoped Gateway route, quota reservation, timeout/merge sweep, and
  idempotent cleanup worker with retry/backoff. See
  [`Preview lifecycle`](../docs/preview-lifecycle.md).

## Acceptance criteria

- Tests cover PR update/reopen/close races, stale commit preview, cross-Tenant route access, expiry, cleanup retries, and duplicate webhooks.
- Catalog reads/writes enforce Tenant RBAC and validate dependency references.
- OIDC token validation rejects wrong issuer/audience/repository/ref/environment/workflow; enabled policies require configured Environment reviewer verification and fail closed when that verifier is unavailable. No cloud credentials are stored or STS exchange enabled.
- Document cost/quota, preview data isolation, cleanup, and production approval policy.
