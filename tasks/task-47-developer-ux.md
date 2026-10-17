# Task 47: Developer CLI and Service Catalog

Status: In Progress. `hako shell <tenant-id> <workspace-id>` requests a short-lived RBAC-authorized shell ticket, connects directly to the TLS WSS Gateway, streams terminal input/output, and attempts session revocation on exit. Cancellation closes the stream cleanly and revokes the session. `hako forward <tenant-id> <workspace-id> <remote-port> [local-port]` binds loopback only, issues/revokes one short-lived ticket per TCP connection, and supports up to 16 concurrent local connections. The versioned PostgreSQL/Redis Service Catalog is wired to authenticated catalog/list/add/action APIs and `hako add`, `hako services`, `hako service start|stop|remove`; changes persist desired state as pending and do not claim to start or physically remove containers. Tasks 44–46 remain partially open; browser IDE, SSH proxy, file transfer, runtime reconciliation/health reporting, and production runtime routing remain. Priority: P1.

## Goal

Complete the basic interactive development flow and reproducible local-service templates.

## Work

- Add CLI shell/SSH proxy, browser IDE open, port forward, file transfer, service list/add/remove, and clear lifecycle progress output.
- Preserve current OAuth PKCE/keyring behavior; never request AWS access keys. Use the Gateway Session protocol, not public SSH.
- Define a versioned Service Catalog schema and initial PostgreSQL/Redis templates with pinned images, resources, volume mounts, health checks, and safe default networks. (Catalog contract and built-ins implemented.)
- Add authenticated Workspace-scoped catalog/list/add/action APIs and CLI with Tenant/Workspace visibility checks, idempotent state transitions, conflict handling, tombstones, and audit records. (Implemented locally; runtime execution remains.)
- Keep development DB local to the Workspace Runtime by default; distinguish explicitly persistent managed DBs.
- Document command syntax, idempotency, user-visible errors, and telemetry.

## Progress

- Implemented `hako shell <tenant-id> <workspace-id>` over the Session API and
  Gateway WSS protocol. The CLI requires a prior `hako login`, `HAKO_API_URL`,
  and `HAKO_GATEWAY_URL`; see [Workspace CLI](../docs/workspace-cli.md).
- Implemented `hako forward <tenant-id> <workspace-id> <remote-port> [local-port]`.
  The local listener is always `127.0.0.1`; each accepted TCP connection gets a
  separate RBAC-issued ticket and WSS stream, with a 16-connection local cap.
- Added a local TLS API/Gateway integration test for ticket issuance, shell
  byte streaming, cancellation, authorization denial, TCP forwarding, and
  session revocation, plus configuration and invalid-port tests. Race tests
  pass for the CLI and Gateway packages.
- Added a versioned built-in Service Catalog (`postgres`, `redis`) with image
  version policy, bounded resources, private-only TCP ports, safe mount paths,
  fixed exec-form health checks, and generated-secret declarations (never
  secret values). Added authenticated Service Catalog, Workspace Service list,
  and add APIs; Tenant membership and Workspace visibility are checked in the
  read/mutation transaction. Identical additions return the same instance;
  another template cannot take over its name. Creation is audited. CLI commands
  are `hako add` and `hako services`.
- The current Service endpoint stores desired-running metadata and returns
  `observed_state=pending`. It is not yet connected to the MicroVM agent, so
  start/health/stop/remove execution, image pulls, generated-secret
  provisioning, and persistent-volume behavior remain incomplete. The start,
  stop, and remove API actions update desired state only. Removal retains a
  name-reserving tombstone and must not be confused with physical cleanup.
  Public ingress is absent.
- This is a first shell transport, not the complete developer UX: reconnectable
  PTY, terminal resize, SSH/VS Code, and file transfer are still open.

## Acceptance criteria

- CLI unit and authenticated integration tests cover happy path, re-login, authorization failure, disconnect, cancellation, and invalid forwards.
- Service tests cover create/start/health/stop/remove, persistent data behavior, resource limits, and image policy.
- No database/service ports are publicly opened and templates reject unsafe privileged settings by default.
