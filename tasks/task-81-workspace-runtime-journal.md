# Task 81: Durable Workspace Runtime Journal and Revision Fence

Status: Implemented locally (Workspace journal model, S3 conditional store, exact launch-request payload pinning, and local restart/concurrency tests; no AWS bucket/IAM deployment). Priority: P1. Parent: Task 43. Depends on: Task 80.

## Goal

Add the Resource Plane's durable per-Workspace state needed to serialize
operations and prevent delayed commands from resurrecting deleted Workspaces.
The Resource Plane must not connect directly to Control Plane DSQL.

## Work

- Define an immutable Workspace identity and monotonically increasing revision
  record with operation identity, lifecycle state, provider binding, and a
  retained deletion tombstone.
- Implement a conditional-create / compare-and-swap store with explicit
  conflict and not-found errors; use S3 conditional requests or another
  reviewed Resource Plane-local durable store.
- Fence stale revisions before provider side effects, and make retries with the
  same operation ID return/recover the same allocation.
- Validate tenant/workspace/resource-plane key construction and payload
  identity; cap and strictly validate serialized records.
- Add IAM/resource-prefix contract documentation. Do not grant Control Plane
  DSQL access or use process memory as the correctness boundary.

## Acceptance criteria

- Unit tests cover initial claim, duplicate operation, stale revision,
  concurrent CAS conflict, delete tombstone, and delayed ensure after delete.
- Store tests cover malformed/mismatched identity, retry after ambiguous write,
  and durable read-after-restart behavior using a local stub.
- Failed cleanup remains represented durably for retry and operator visibility.

## Result

- Added `internal/workspacejournal` with strict identity/schema validation,
  launch token/spec pinning, independent Workspace/CAS revisions, allocation
  generations, operation admission, and irreversible delete tombstones.
- Added an S3 store using conditional object creation and ETag CAS; object keys
  are scoped by Resource Plane, Tenant, and Workspace. Runtime store access is
  read/write only and never deletes journal objects.
- Unit tests cover duplicate/stale commands, concurrent CAS, delete-then-stale
  ensure, replacement only after confirmed termination, malformed/foreign
  payloads, and ambiguous-write recovery after simulated controller restart.
- [Journal storage/IAM contract](../docs/workspace-runtime-journal.md) records
  the S3 prefix and least-privilege deployment boundary. Real bucket, role, and
  AWS conditional-write behavior are not deployed/verified.
- Task 82 completed the provider adapter and the Resource Controller Lambda
  locally composes it with this journal. The optional Terraform deployment
  configures the `lambda-microvm` runtime and scoped journal/provider IAM but
  remains default-off and unapplied; production execution is not enabled.
