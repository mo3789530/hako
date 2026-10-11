# Task 82: Lambda MicroVM Workspace Runtime Adapter

Status: Todo. Priority: P1. Parent: Task 43. Depends on: Tasks 80 and 81.

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
- Apply the required ownership/provenance tags and verify returned resources
  belong to the requested Tenant, Workspace, and Resource Plane.
- Keep AWS credentials in the Resource Plane execution role; never accept
  credentials in commands or persist them in the journal.

## Acceptance criteria

- SDK-stub tests cover exact launch parameters, lifecycle state mapping,
  duplicate delivery, timeouts, unsupported operations, and API errors.
- Stale/tombstoned commands are rejected before any provider API call.
- No production configuration enables the adapter before the required image,
  network, and lifecycle PoC gates are satisfied.
