# Task 43: Production Workspace Runtime

Status: In Progress (decomposed into Tasks 80–83; no Workspace production runtime is implemented yet). Priority: P1.

## Goal

Replace the production gap behind the current fake-only Resource Controller with an AWS Runtime that implements ensure-running, suspend, resume, and delete without weakening the existing Workspace revision-fencing contract.

## Execution units

Implement in dependency order; do not count this parent together with its child tasks:

1. [Task 80: Runtime contract and capability ADR](task-80-workspace-runtime-contract.md)
2. [Task 81: Durable journal and revision fence](task-81-workspace-runtime-journal.md)
3. [Task 82: Lambda MicroVM Runtime adapter](task-82-workspace-lambda-microvm-adapter.md)
4. [Task 83: Reconciliation and local fake-provider E2E](task-83-workspace-runtime-recovery-e2e.md)

Task 61's Workload Job adapter is not a Workspace runtime and does not satisfy
this parent task. Keep real AWS PoC, image/network proof, and deployment
approval as separate gates; EC2 remains out of scope.

## Acceptance criteria

- Tasks 80–83 acceptance criteria pass and are individually documented/tested.
- Runtime adapter uses durable per-Workspace revision/tombstone fencing before
  provider effects, minimal IAM, and tags from `docs/aws-resource-tags.md`.
- Provider API integration runs against a local fake; no AWS apply is implicit.
- AWS image/container/network/lifecycle PoC remains explicitly unverified until
  approved and completed in a real Resource Plane account.
