# Cognito identity mapping and Tenant RBAC

## Identity mapping

The API first validates the Cognito access token, then resolves its verified `sub` through `users.ResolveCognitoSubject` to a stable Hako `User.id`. Cognito `sub` is the external identity key; email is not an identity key and is deliberately not inferred from the access token. Profile/email synchronization requires a separately verified source and is not part of this step.

The resolver uses `(cognito_subject)` uniqueness and an idempotent upsert. Its generated Hako User ID remains stable across OCC transaction retries. Cognito Groups are not mapped to Tenant roles; Tenant authorization is controlled only by `tenant_members`.

Resolving a Cognito identity does **not** automatically create Tenant membership. An authenticated user with no membership receives no Tenant access. Tenant invitations/creation and membership administration are separate operations.

## Tenant membership guard

`authz.RequireTenantMembership` reads the role for `(tenant_id, user_id)` and returns the `ErrTenantAccessDenied` sentinel when membership is absent. It intentionally gives the same denial for a missing Tenant and a non-member to reduce Tenant ID enumeration. `RequireTenantRole` can restrict an operation to an explicit set of roles; an empty role list allows any Tenant member.

Call membership authorization in the same transaction as the protected resource read or write where practical, so resource access and membership evaluation share a consistent transaction boundary. Tenant Owner/Admin/Member roles come from DSQL, not client claims.

## API route and CLI

`GET /v1/tenants/{tenant_id}/membership` is the first Tenant-scoped route. The Echo middleware order is:

1. Validate Cognito access token.
2. Require the `hako/api` scope.
3. Resolve verified Cognito `sub` to the stable Hako User record.
4. Require a `tenant_members` row for that User and Tenant.
5. Return the caller's Tenant role.

Missing Tenants and non-members both return the same `404` error envelope, `{"error":{"code":"not_found","message":"Tenant not found"}}`. Database failures return a generic `503`; database details are not exposed. A valid user without the required OAuth scope is rejected before a Hako User is resolved/created.

The CLI command `hako tenant membership <tenant-id>` calls this route with the access token from the OS credential store and prints the Tenant ID, Hako User ID, and role. It does not print token material. The API origin is configured via `HAKO_API_URL`.

Future Tenant-scoped handlers must apply the same authenticated Hako User and membership guard before reading or changing tenant-owned data. For mutations, membership checking should be in the same transaction as the protected operation where practical.

Workspace list/get routes re-read Tenant membership inside the same transaction as the data query. Members see only rows with their `owner_id`; Owners and Admins see all rows for that Tenant. A missing or non-visible Workspace returns the same 404 response. Cognito Groups and client-provided owner IDs never grant workspace visibility.

## Workspace visibility

Workspace visibility is implemented for the list and get APIs: Member sees only Workspace rows they own, while Owner/Admin see all Workspace rows within the Tenant. The same transaction rechecks membership and applies the visibility filter. Session issuance and other future Workspace entry points must reuse this policy; they are not implemented yet.
