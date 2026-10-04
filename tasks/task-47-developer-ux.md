# Task 47: Developer CLI and Service Catalog

Status: Blocked on Tasks 44–46. Priority: P1.

## Goal

Complete the basic interactive development flow and reproducible local-service templates.

## Work

- Add CLI shell/SSH proxy, browser IDE open, port forward, file transfer, service list/add/remove, and clear lifecycle progress output.
- Preserve current OAuth PKCE/keyring behavior; never request AWS access keys. Use the Gateway Session protocol, not public SSH.
- Define a versioned Service Catalog schema and initial PostgreSQL/Redis templates with pinned images, resources, volume mounts, health checks, and safe default networks.
- Keep development DB local to the Workspace Runtime by default; distinguish explicitly persistent managed DBs.
- Document command syntax, idempotency, user-visible errors, and telemetry.

## Acceptance criteria

- CLI unit and authenticated integration tests cover happy path, re-login, authorization failure, disconnect, cancellation, and invalid forwards.
- Service tests cover create/start/health/stop/remove, persistent data behavior, resource limits, and image policy.
- No database/service ports are publicly opened and templates reject unsafe privileged settings by default.
