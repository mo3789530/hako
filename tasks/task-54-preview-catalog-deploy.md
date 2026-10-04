# Task 54: Preview Workspaces, Software Catalog, and OIDC Deploy Policy

Status: Blocked on Tasks 43–46 and 50–51. Priority: P3.

## Goal

Add optional repository-level preview environments and service metadata/deploy policy after the Workspace Runtime and Gateway are production-ready.

## Work

- Create one isolated Preview Run/Workspace per authorized PR commit; expose only through Gateway with expiring Tenant-scoped routing; tear down on PR close/merge/timeout.
- Add a Tenant-scoped Software Catalog mapping service, owner/team, repository, runtime/language, dependencies, and production URL without duplicating secret data.
- Add GitHub OIDC cloud identity only for trusted release/deploy Workloads; constrain cloud trust by Repository, ref/environment, and verified workflow claims.
- Require explicit human approval and policy checks for production; untrusted forks and agents receive no deploy role.

## Acceptance criteria

- Tests cover PR update/reopen/close races, stale commit preview, cross-Tenant route access, expiry, cleanup retries, and duplicate webhooks.
- Catalog reads/writes enforce Tenant RBAC and validate dependency references.
- OIDC token validation rejects wrong issuer/audience/repository/ref/environment and never stores cloud credentials.
- Document cost/quota, preview data isolation, cleanup, and production approval policy.
