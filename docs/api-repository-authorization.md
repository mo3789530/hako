# API repository authorization

Status: implementation started. Grant storage, repository action checks and grant administration are available in this branch. Existing resource routes retain their current Tenant RBAC until the enforcement rollout. This design covers Control Plane API authorization independently of AgentCore and runtime isolation.

## Current behavior

The [Tenant RBAC policy](tenant-identity-and-rbac.md) resolves a verified Cognito identity to a Hako user and checks Tenant membership. Repository registration binds a GitHub installation and repository to a Tenant. Workload Run creation checks that binding. Preview session issuance checks Tenant membership and the requested Preview allocation. Neither check determines whether the user has access to that specific repository. Workload Run reads use Tenant role or requester ownership.

## Policy and data model

Use the canonical repository key `(tenant_id, installation_id, github_repository_id)` from the registry. Add a `tenant_repository_members` table keyed by that tuple and `user_id`, with a role, grant actor and timestamps. Keep grants in Hako DSQL. A grant requires current Tenant membership and an active repository binding. GitHub App installation permissions establish what Hako can access from GitHub. They do not grant permissions to a Hako user.

| Principal | Read repository resources and use Preview | Start runs | Manage repository configuration and grants |
| --- | --- | --- | --- |
| Tenant Owner or Admin | All active repositories in the Tenant | Yes | Yes |
| Repository Viewer | Granted repositories | No | No |
| Repository Operator | Granted repositories | Yes | No |
| Other Tenant members | Denied | Denied | Denied |

Keep the existing own-run and own-Workspace rules for ordinary members. Require repository access in addition to ownership. Keep publication approvals, model credential administration and other privileged operations subject to their existing stricter policies. Explicit cross-Tenant platform operations remain on their separately authenticated and audited administrative routes.

Use existing Tenant-scoped routes and add owner/admin routes to list, set and delete repository grants under `/v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/members`. The API must validate the target user's Tenant membership and registered repository before granting access. Audit grant changes in the same transaction and record Tenant, installation, repository, actor, target user and role change. Delete grants when a member is removed or a repository binding is removed or reassigned. Require a fresh grant on rejoining or rebinding.

Grant administration is exposed when GitHub App management is enabled. Use `GET /members` to list current grants, `PUT /members/{user_id}` with `{"role":"viewer"}` or `{"role":"operator"}` to set a grant, and `DELETE /members/{user_id}` to revoke it. A grant is tied to the membership join time and installation binding time so it becomes invalid when either changes. Repository removals processed by GitHub webhooks delete affected grants in the same transaction.

## Enforcement

- Centralize decisions in `internal/authz`. Resolve membership, active registration and action permission in the same transaction as the protected read or write. Uniformly return repository/resource `404` for missing or invisible objects, and `403` for a visible object when an action exceeds the user's role. Fail closed on authorization store errors.
- Apply the decision to repository listings and settings, run creation and lifecycle operations, Preview listing and sessions, repository-backed Workspaces and sessions, run artifacts, publication endpoints, and repository references exposed by catalog APIs. Derive the repository for child objects from the stored parent row within the requested Tenant. Validate any route repository against that identity. Apply visibility filters in SQL before counting or paginating. Tenant-only resources continue to use Tenant RBAC.
- On revocation, deny new access and invalidate affected live session leases. Recheck current policy when opening or renewing a session, and enforce a bounded recheck interval for active connections. Replace direct artifact URLs with authenticated downloads where immediate revocation is required. Otherwise bound and document the lifetime of issued URLs. An independently authorized worker may finish an accepted job under its persisted scope. Revocation removes the user's access to its output and ability to take further actions.
- Webhooks, credential brokers and execution callbacks use separate service authentication. Bind each operation to trusted Tenant, installation, repository and run or attempt metadata, using the existing service-specific checks. Human ACL entries cannot authorize a machine request. Revalidate current binding before issuing source credentials or allowing publication.

## Rollout and acceptance

1. Add grant storage, administration and audit. Prepare explicit grants for current users and repository owners. Preserve Tenant Owner/Admin access under the stated policy. Do not infer grants from prior activity.
2. Implement and inventory all affected routes before enforcement. Compare proposed and current decisions in staging, including list visibility, then apply enforcement across all affected routes together. Treat databases with no imported grants as deny-by-default for ordinary members.
3. Add database-backed integration tests with two Tenants and two repositories in one Tenant. Cover same-Tenant access to the wrong repository, mismatched installation and repository IDs, forged parent/child IDs, list items and totals, Viewer/Operator differences, ownership checks, admin scope, membership removal and rejoin, binding changes, and revocation of active sessions. Test that a service request cannot switch execution scope by supplying another repository ID.

The existing Cognito issuer and client suffice for this design. If Tenant-specific User Pools are introduced later, verify issuer and client against trusted configuration and map identities by `(issuer, sub)`. The repository policy remains a Control Plane decision.
