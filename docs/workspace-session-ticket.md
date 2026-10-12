# Workspace Session Ticket

The Control Plane can issue a short-lived signed ticket only after looking up
the caller's Tenant membership and Workspace visibility in the same database
transaction. Members can access only their own Workspace; Tenant owners/admins
follow the existing `GetVisible` policy. Issuance is allowed only while both
desired and observed Workspace states are `running`, and the Placement is read
from the database rather than accepted from the request.

Tickets use Ed25519 and are audience-bound to `hako-gateway`. Claims bind the
user, Tenant, Workspace, Resource Plane, allowed channels (`shell`, `vscode`,
`port-forward`), issue/expiry times, and a random 192-bit ticket ID. Maximum
TTL is five minutes. API issuance persists the ticket ID and permissions in
the existing `sessions` table and appends an audit event in the same
transaction. The response ticket is never included in audit details. A
new issuance also prunes that user's expired session rows in the same Tenant.
A Workspace owner or Tenant owner/admin can revoke a session through the API;
revocation deletes the active session record and writes a second audit event.

`workspacesession.Verifier` verifies the signature against a configured key
set, requires the exact audience, checks expiry and requested channel
permission, and calls an injected `ActiveSessionLookup` before authorizing the
connection. `HTTPActiveSessionLookup` is the local HTTPS client implementation;
it signs the complete claims body, method, path, timestamp, and random request
ID with a per-Resource-Plane HMAC key. The Control Plane route
`POST /v1/internal/resource-planes/{id}/workspace-sessions/active` validates
that signature and compares ticket ID, audience/version, expiry, permissions,
and the complete User/Tenant/Workspace/Resource Plane identity against the
persisted `sessions` row. It returns only `{ "active": true|false }` and
never connects the Resource Plane to Aurora DSQL. Duplicate reads are
idempotent; timestamp freshness bounds replay, and the request ID is for
correlation rather than a durable one-time nonce. Transport uses normal HTTPS
certificate validation and redirects/proxies are disabled in the default
client.

The API enables this endpoint only when configured with
`HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS`, a JSON map from Resource
Plane ID to a distinct Secrets Manager ARN. The API resolves keys only for the
matching Plane. The production Secrets, IAM grants, Resource Plane client
configuration/distribution, and live revocation propagation are not yet
provisioned. A Resource Plane Gateway must not connect directly to Aurora DSQL.
The Gateway must route from signed Tenant/Workspace/Resource Plane claims;
caller-supplied routing IDs must not override those claims. Key rotation should publish the new public key
before switching the Control Plane signer, keep the previous public key until
all tickets signed by it expire, then remove it. For incident response, disable
the signing key and revoke affected session rows; existing data streams also
need an active-connection close mechanism, which remains to be implemented.

Ticket issuance is presently dependency-injected into the API handler and is
disabled if no issuer is configured. Production signer/lookup-key provisioning,
Gateway client configuration, active-connection revocation propagation,
fleet-wide single-connection/replay coordination, WSS routing, rate/connection limits, metrics, and the
real MicroVM upstream path remain open Task 46 work. The ticket library,
RBAC-backed API flow, HTTPS/HMAC lookup client, and PostgreSQL-backed lookup
endpoint are locally tested; they do not constitute a deployed Zero Trust
Gateway.
