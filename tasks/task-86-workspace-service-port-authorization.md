# Task 86: Workspace Service port authorization

Status: Local implementation and PostgreSQL integration acceptance complete.
Production Gateway rollout remains an operational deployment gate.

## Goal

Allow an authorized Workspace Session to forward only TCP ports that belong to
healthy Services in the Workspace's current Lambda MicroVM allocation.

## Design

- Keep the Gateway as the Data Plane. It asks the Control Plane for a boolean
  authorization decision over signed HTTPS using the existing per-Resource-
  Plane Session Lookup HMAC key; it never sends terminal or application bytes
  through the Control Plane.
- Bind the decision to the complete persisted Session identity and
  `port-forward` permission. Require the Workspace to be desired and observed
  running, plus a Service that is desired running, observed healthy at its
  current revision, and attached to the active MicroVM allocation generation.
- Resolve allowed ports from the validated Service Catalog configuration.
  Never accept a host, arbitrary destination, public ingress, or caller-defined
  port declaration.
- Keep Preview authorization separate: its fixed application port is permitted
  only for the active generation-specific Preview identity. Ordinary Workspace
  Services use the Control Plane authorization endpoint.
- Fail closed on invalid HMAC, unavailable Control Plane state, stale runtime,
  unhealthy Service, suspended Workspace, or undeclared port.

## Work

- [x] Add Resource Plane HMAC-authenticated internal port authorization API.
- [x] Verify persisted Session claims, permission, expiry, Tenant/Workspace/
  Resource Plane identity and running Workspace state.
- [x] Authorize only TCP ports from healthy current-generation Service catalog
  entries; fail closed for unhealthy or inconsistent runtime observations.
- [x] Wire Gateway client and Preview/ordinary Workspace policy chain using the
  existing mounted per-Plane HMAC key.
- [x] Document the Gateway-to-Control-Plane metadata authorization boundary.
- [x] PostgreSQL integration coverage: declared/undeclared port, retired
  generation, unhealthy Service, suspended Workspace, moved Placement, and
  claim mismatch.

## Verification

- `go test -p=1 ./internal/store/services ./internal/api ./internal/workspacesession ./internal/workspacegateway ./cmd/hako-workspace-gateway -count=1`
- `HAKO_TEST_DATABASE_URL=... go test -p=1 -tags=integration ./internal/api -run '^TestResourcePlaneWorkspacePortAuthorizationUsesHealthyCurrentServices$' -count=1`

No AWS apply, Secret retrieval, DNS change, external cutover, or real MicroVM
operation is part of local acceptance.
