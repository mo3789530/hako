# Task 82: Lambda MicroVM Workspace Runtime Adapter

Status: Implemented locally (SDK-injected Lambda MicroVM lifecycle adapter, trusted spec resolver, conditional journal fencing, frozen launch request/retry, state verification, and SDK-stub tests; not wired into production Lambda). Image/network/lifecycle PoC and AWS deployment remain gated. Priority: P1. Parent: Task 43. Depends on: Tasks 80 and 81.

## Goal

Implement the production `resourcecontroller.Runtime` adapter for the
Workspace-specific Lambda MicroVM API, using the durable journal and the
official AWS SDK. This is distinct from the existing Workload Job adapter.

## Work

- Add an injectable MicroVM API client and explicit operation deadlines.
- Implement only lifecycle operations verified by Task 80; reject unsupported
  operations/runtime classes before making provider calls.
- Use stable idempotency parameters for ambiguous launch responses and recover
  existing provider IDs from the journal/provider API without creating a
  second allocation.
- Apply ownership/provenance tags where the provider API supports them and
  verify returned resources belong to the requested Tenant, Workspace, and
  Resource Plane. Task 80 found no verified per-MicroVM tag support, so the
  immutable Resource Plane journal remains the ownership record; do not adopt
  unjournaled provider resources.
- Keep AWS credentials in the Resource Plane execution role; never accept
  credentials in commands or persist them in the journal.

## Acceptance criteria

- SDK-stub tests cover exact launch parameters, lifecycle state mapping,
  duplicate delivery, timeouts, unsupported operations, and API errors.
- Stale/tombstoned commands are rejected before any provider API call.
- No production configuration enables the adapter before the required image,
  network, and lifecycle PoC gates are satisfied.

## Local result

Implemented in `internal/workspaceruntime` and documented in
[`docs/workspace-runtime-adapter.md`](../docs/workspace-runtime-adapter.md).
Exact launch parameters are persisted in the journal so ambiguous retries
cannot silently change image/network/role settings after a process restart.
The Resource Controller Lambda remains fake-only. SDK stubs verify launch,
duplicate delivery, ambiguous response recovery, state transitions, unsupported
runtime class/provider errors, stale-command rejection, and poll deadlines;
no AWS call was made.
