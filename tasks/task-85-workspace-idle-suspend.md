# Task 85: Workspace connection leases and idle suspension

Status: Local acceptance complete; real MicroVM and AWS deployment remain. The CLI/Guest shell keeps a PTY alive across a transient
WSS disconnect within the same Lambda MicroVM, and the Gateway revokes stale
sessions during an established stream. A durable connection-lease schema and
store now bind leases to Tenant/Workspace/Resource Plane/MicroVM/allocation
generation, cap lease duration at two minutes, prevent released-lease revival,
and select idle candidates only from a previously connected, currently running
allocation with no active lease. Local PostgreSQL integration covers renew,
release, expiry, Tenant mismatch, and replacement-generation fencing. The
Control Plane now exposes HMAC-authenticated Resource Plane lease renew/release
endpoints and the PostgreSQL API E2E verifies signature rejection, idempotent
release, and fail-closed renewal after release. The signed Resource Plane
client is wired into the Lambda MicroVM Gateway: it creates an exact-allocation
lease before Guest WSS, refreshes every 30 seconds (90-second TTL), closes the
stream on renewal failure, and releases on close. Tenant Owner/Admin GET/PUT
policy API and CLI are implemented with audit, a disabled-by-default policy,
15-minute initial grace, and a 5-minute–24-hour bound. Gateway→API→PostgreSQL
lifecycle and policy authorization are integration-tested. The transactional
idle sweep now rechecks policy/allocation/leases and emits the normal suspend
Operation, Outbox, and system-actor audit; it has an EventBridge Lambda
entrypoint. A composed local E2E drives Gateway WSS through signed lease
renew/release API calls into PostgreSQL, proves reconnect fences an idle sweep,
and verifies an expired disconnect produces one suspend Operation, Outbox, and
system audit. A default-off Control Plane Terraform schedule, exact DSQL
`DbConnect` role, SQL role grants, and Lambda error alarm are defined and pass
local Terraform validation. The CLI supports `hako resume ... --wait` and
retains the default asynchronous mode. Real MicroVM Guest reconnect behavior,
and AWS deployment remain. The composed local E2E now also dispatches the idle
suspend through the standard Outbox Dispatcher, routes it to the Placement
Resource Plane queue, executes it through the Resource Controller fake, and
applies its terminal result back to PostgreSQL. See
[Workspace connection lifecycle](../docs/workspace-connection-lifecycle.md).
Priority: P1. Depends on Tasks 43–47 and the Task 84 Workspace binding/generation fence.

The local composed Gateway lease/sweep and CLI option checks run with
`make test-workspace-idle-lifecycle-integration` against the already-running
development PostgreSQL database.

## CLI API wiring refresh (2026-10-24)

The Tenant idle-policy CLI now has an HTTP-backed test for both GET and PUT.
It checks the exact Tenant endpoint, Bearer token, serialized enabled/grace
policy, returned revision, and that the CLI Tenant argument overrides a
Tenant ID embedded in the update object. Missing token/API URL fail closed.
This complements the PostgreSQL API authorization/audit integration tests.

## Goal

Implement the documented lifecycle:

```text
connected → disconnected → grace period → idle → suspended
```

A network loss must not immediately terminate PTYs, containers, or local
development databases. After the configured grace interval, Hako may request
the existing asynchronous suspend Operation, but only if there are no active
Workspace connections and the Workspace allocation has not changed.

## Design constraints

- Keep terminal, application, and database payloads on the Data Plane. The
  Control Plane receives only connection lease metadata and timestamps.
- Do not let the Gateway call Lambda MicroVM APIs or mutate Workspace desired
  state directly. It reports connection leases; a Control Plane idle reconciler
  emits the existing suspend Operation and Resource Plane Outbox command.
- A lease is scoped to Tenant, Workspace, Resource Plane, MicroVM ID, and
  allocation generation. A replacement runtime invalidates all prior leases.
- Use an opaque, short-lived lease ID derived from Gateway-generated random
  data, never a client-supplied identifier. Authenticate Resource Plane reports
  with the existing per-Plane HMAC boundary and compare the complete identity.
- Refresh leases while a WSS connection is active. Expired leases must be
  recoverable after Gateway, Lambda, or Control Plane restarts; process-local
  timers are not authoritative.
- Create at most one suspend Operation for an idle Workspace revision. Manual
  suspend/delete, active connections, a newer desired-state revision, or a new
  allocation must fence the idle request.
- Record an explicit system actor/reason in audit metadata. Do not impersonate
  a Tenant member or reuse a user's Session ticket as an automation identity.
- Keep the policy and default explicit. Existing Tenants must not be silently
  moved to automatic suspension during rollout.

## Work

1. [x] Define a Tenant-level idle policy (enabled flag and bounded grace
   interval) with an explicit disabled default and Owner/Admin API/CLI management.
2. [x] Add a durable connection-lease table and migration. Upsert/renew and
   release operations are idempotent and generation-fenced; PostgreSQL tests
   cover allocation identity and expiry behavior.
3. [x] Add the signed Resource Plane lease client and wire it into the Gateway's
   stream lifecycle. Connection establishment creates a lease before Guest WSS;
   a 30-second heartbeat renews its 90-second TTL, renewal failure closes the
   stream, and close/error paths release it. Missed release is recovered by
   expiry. The Control Plane HMAC renew/release endpoints and store transaction
   are implemented.
4. [x] Implement the Control Plane idle sweep and EventBridge Lambda entrypoint.
   It identifies expired idle Workspaces and atomically appends the normal
   suspend Operation, system-actor audit event, operation event, and Outbox
   command. It reuses lifecycle fencing and never calls provider APIs. The
   default-off Terraform schedule/IAM contract and alarm are locally validated.
   AWS deployment remains an operations gate.
5. [x] Document and implement explicit resume UX: `hako resume ... --wait`
   waits for observed running state; default resume remains asynchronous and
   `hako shell`/`hako open` do not resume implicitly.
6. [x] Document grace-period semantics, active-connection definition, policy
   defaults, crash recovery, manual override, and local-disk loss on MicroVM
   termination.
7. [x] Exercise idle suspend through the standard Dispatcher, Resource
   Controller, and Operation result consumer with fake queue/runtime adapters;
   verify Outbox acknowledgement and terminal `observed_state=suspended`.

## Acceptance criteria

- PostgreSQL tests prove lease renewal/release idempotency, expiry recovery,
  cross-Tenant/Plane denial, wrong-MicroVM/generation rejection, and concurrent
  connection races.
- A disconnect followed by reconnect before the grace deadline does not
  suspend the Workspace; PTYs and the same MicroVM allocation remain available.
- At/after the grace deadline, the reconciler creates exactly one suspend
  Operation only when the Workspace is still desired/observed running and has
  no unexpired leases.
- Manual suspend/delete, new connection, changed policy, active operation, or
  replacement generation wins races and prevents a stale idle Operation.
- Gateway restart and missed release are recovered through durable expiry;
  no process-local timer is required for correctness.
- Integration tests exercise Gateway lease events through PostgreSQL and the
  standard Dispatcher/Resource Plane suspend path using fakes, including
  Outbox routing/publication, fake runtime execution, and result projection.
- No terminal/application bytes enter the Control Plane. Logs contain no
  Session Ticket, Agent Ticket, lease secret, or terminal payload.
- Keep PostgreSQL running. No real Secret retrieval, AWS apply, external
  cutover, or real MicroVM action is part of local acceptance.
