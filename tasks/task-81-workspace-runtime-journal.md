# Task 81: Durable Workspace Runtime Journal and Revision Fence

Status: Todo. Priority: P1. Parent: Task 43. Depends on: Task 80.

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
