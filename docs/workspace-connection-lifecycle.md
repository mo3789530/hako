# Workspace connection leases and idle suspension

Hako separates a local network connection from the Workspace and its Lambda
MicroVM lifecycle. A WSS disconnect must not immediately terminate a PTY,
container, or Workspace-local development database. The intended transition is

```text
connected → disconnected → configured grace period → idle → suspended
```

The full automatic lifecycle is tracked by
[Task 85](../tasks/task-85-workspace-idle-suspend.md). The Gateway reports
connection leases, and an enabled Tenant policy is acted on by the idle sweep.
Manual `suspend` remains available as an explicit override. The EventBridge
Lambda entrypoint and default-off Terraform schedule/IAM contract exist. The
schedule has not been enabled or applied in AWS.

## Policy semantics

Automatic suspension will be an explicit Tenant-level opt-in and will default
to disabled for existing and new Tenants. The initial grace-period default is
15 minutes when enabled, with an allowed range of 5 minutes to 24 hours. The
policy applies to all eligible Workspaces in the Tenant. A Workspace counts as
connected while it has an unexpired, allocation-matching Gateway lease. Manual
suspend/delete and an active user connection take precedence over idle work.
The policy API, storage, and automated suspend action are implemented locally.
An idle lease means there is no live Gateway connection, not that the Workspace
has no running process. During the grace period, the PTY and processes remain
on the same MicroVM. Once suspension terminates a MicroVM, data on its local
disk is not guaranteed to survive; source files belong on the EFS-backed
Workspace path when they must persist.

Owner/Admin can inspect or configure the Tenant policy:

```bash
hako tenant workspace-idle-policy get <tenant-id>
hako tenant workspace-idle-policy set <tenant-id> true 900
```

The GET/PUT API is `/v1/tenants/{tenant-id}/workspace-idle-policy`. Updates
are audited and revisioned. The idle sweep reads this setting while it
revalidates each candidate.

Opening a suspended Workspace requires an explicit resume. To return as soon
as the runtime reports `running`, use
`hako resume <tenant-id> <workspace-id> --wait`; without `--wait`, resume
returns after the asynchronous Operation is accepted. The equivalent explicit
sequence is `hako resume ...` followed by
`hako wait <tenant-id> <workspace-id> --state running`. `hako shell` and
`hako open` do not implicitly resume a Workspace.

## Durable lease contract

`workspace_connection_leases` stores only the opaque connection lease ID,
Tenant/Workspace/Resource Plane IDs, Lambda MicroVM ID, allocation generation,
expiry, release time, and renewal timestamps. It stores no Session/Agent
Ticket, user credentials, terminal data, or application payload.

Lease creation and renewal require the current DB Placement and the exact
running `lambda_microvm` resource binding. A lease ID cannot move across a
Tenant, Workspace, Resource Plane, MicroVM, or allocation generation, and a
released lease cannot be revived. Renewals are bounded to two minutes. The
Gateway opens a lease before dialing Guest WSS, renews every 30 seconds with a
90-second expiry, and releases it when the stream closes. A failed heartbeat
closes the upstream stream. Expiration recovers a release missed during process
failure.

## Shared Gateway connection admission

Each Lambda MicroVM Gateway opens a signed metadata lease before it dials the
Guest WSS endpoint. The Control Plane admits at most ten unexpired leases for
the current Workspace allocation across all Gateway replicas and returns
HTTP 429 above that limit. An existing lease can renew while the allocation
is full; release and expiry free capacity. Admission uses the Workspace status
row as an Aurora DSQL OCC conflict fence, then counts and writes in the same
transaction. On a serialization conflict, the transaction retry repeats the
count before attempting the insert. Aurora DSQL adjudicates `SELECT FOR UPDATE`
reads through OCC at commit time rather than holding a traditional row lock;
see [Aurora DSQL concurrency control](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/working-with-concurrency-control.html).
The API handles lease metadata only; it does not carry Workspace application
traffic.

This cap is not configurable yet. Per-Gateway process limits can be lower.
The same shared Workspace status row also enforces a fixed-window start-rate
limit of 30 new connection leases per minute across Gateway replicas. Existing
lease heartbeats do not consume a new-start slot. Capacity and rate-limit
denials are persisted/returned as HTTP 429; the rate counter saturates one
above the threshold until the window expires.

## Cross-replica Session replay fence

Lease renewal also atomically claims the exact, unexpired Session row identified
by the signed ticket ID, Tenant, Workspace, and Resource Plane. One Session can
have only one active connection lease at a time, including when requests reach
different Gateway replicas. A competing lease receives HTTP 409. Releasing a
lease clears the claim only when that lease owns it; if a Gateway disappears,
the claim becomes reusable at the earlier of the lease expiry and Session
expiry. The signed ticket value and its claims are not stored in this field.
This is connection replay exclusion; the fixed-window counter above limits
new lease admissions, not terminal or application traffic.

Idle candidate selection is scoped to the current running allocation. It
excludes Workspaces with any unexpired lease and Workspaces that have never
had a connection lease, so the idle sweep cannot suspend a new or
never-connected Workspace by accident. Candidate selection is advisory: before
creating a suspend Operation, the reconciler must recheck the exact binding,
policy, desired/observed state, active leases, and pending Operations in the
same transaction that writes the existing Operation, audit, event, and
Outbox. Manual suspend/delete and a replacement allocation must win races.

The Control Plane now exposes HMAC-authenticated Resource Plane `PUT` renew
and `POST .../release` endpoints. Both validate the exact Plane, bounded body,
timestamp/signature, and database-fenced allocation; the same lease ID is the
idempotency key, and a released lease cannot be revived. PostgreSQL integration
tests cover renew/release, invalid signature, and replay after release.

This document describes the locally implemented idle-suspend path. A composed
local E2E drives a signed Session ticket through Gateway WSS, reports
disconnect/reconnect leases through the signed Control Plane API into
PostgreSQL, proves an active reconnect fences suspension, and verifies that an
expired idle lease creates exactly one suspend Operation, Outbox event, and
system audit. The upstream runtime is a fake stream; real MicroVM Guest
reconnect behavior remains open. The default-off
Terraform schedule, IAM, DSQL role contract, and alarm pass local validation;
AWS deployment remains open. No AWS apply or real Secret read is part of local
verification.

Run the composed Gateway/API/PostgreSQL lifecycle and CLI resume option tests
against the already-running local PostgreSQL with
`make test-workspace-idle-lifecycle-integration`.
