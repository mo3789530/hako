# Task 80: Workspace Runtime Contract and Capability ADR

Status: Todo. Priority: P1. Parent: Task 43. Depends on: Task 61 runtime evidence.

## Goal

Define the Workspace-specific Lambda MicroVM lifecycle contract before adding
provider behavior. Task 61 validates the workload-job runtime; this task records
which of those capabilities are also valid for long-lived interactive
Workspaces and which remain unsupported.

## Work

- Record supported operations (`ensure_running`, `suspend`, `resume`, `delete`),
  state mapping, deadlines, retry behavior, and provider errors.
- Verify the official API/SDK supports each operation and document maximum
  lifetime, architecture, image, network, and connector constraints.
- Decide how an interactive Workspace survives provider maximum-duration
  termination and whether a replacement VM is safe without Task 44 persistence.
- Specify Workspace revision and deletion-tombstone semantics for all commands.
- Keep unsupported runtime classes/capabilities fail-closed; do not imply that
  a Lambda invocation is a durable Workspace.

## Acceptance criteria

- ADR and runtime documentation state verified capabilities separately from
  assumptions and unverified AWS PoC items.
- Lifecycle/state mapping and idempotency requirements are precise enough for
  the journal and adapter subtasks.
- No AWS apply or real Workspace launch is required for completion.
