# Task 43: Production Workspace Runtime

Status: Implemented locally through Tasks 80–83 (contract, durable journal, Lambda MicroVM adapter, SDK HTTP/recovery and PostgreSQL result/binding E2E). The Resource Controller Lambda composes the AWS runtime behind an explicit `HAKO_RUNTIME_IMPLEMENTATION=lambda-microvm` mode with strict static launch-profile parsing and the S3 journal. Resource Plane Terraform now has a default-off ARM64 Lambda deployment contract with exact image/journal permissions, dynamic EFS-role prerequisite, and mutually exclusive command-queue mapping; it remains unset in the dev root. Real AWS image/network/lifecycle PoC, EFS/agent recovery, and deployment remain unverified and gated by Tasks 44/45/61. Priority: P1.

## Goal

Replace the production gap behind the current fake-only Resource Controller with an AWS Runtime that implements ensure-running, suspend, resume, and delete without weakening the existing Workspace revision-fencing contract.

## Local controller composition progress

- Added an explicit Lambda MicroVM mode to the Resource Controller Lambda.
  It composes the official MicroVM SDK client, S3 conditional Workspace
  journal, a strict launch contract, and the existing revision-fenced
  runtime. Terraform remains hard-coded to the development Fake Runtime and
  its IAM policy grants neither S3 journal nor MicroVM API access. A separate
  Terraform opt-in now composes the same handler with the Lambda MicroVM
  runtime, but the dev environment leaves it unset and no AWS permissions or
  resources have been applied.
- The launch profile is strict, bounded Resource Plane configuration; commands
  cannot select an image, execution role, or connector. Profile validation
  requires the image/connectors to match the Lambda region and execution-role
  account, and rejects Workspace-specific hook data. Local tests verify the
  composition/config boundary only; they make no AWS calls.
- Fixed Workspace command routing in the Outbox Dispatcher to accept both
  legacy schema v1 and revision-fenced schema v2 envelopes (requiring a
  positive revision for v2); previously actual create/action commands written
  as v2 were rejected by a v1-only route validator. The SQS message body remains
  unchanged for the strict Resource Plane controller decoder.
- This does not satisfy the live AWS deployment gate: agent startup and EFS
  persistence are not wired, and no MicroVM/network/image PoC or apply has been
  performed. Do not enable this mode in Terraform before Task 44/45 and 61
  gates are reviewed.

## Execution units

Implement in dependency order; do not count this parent together with its child tasks:

1. [Task 80: Runtime contract and capability ADR](task-80-workspace-runtime-contract.md) — complete
2. [Task 81: Durable journal and revision fence](task-81-workspace-runtime-journal.md) — implemented locally
3. [Task 82: Lambda MicroVM Runtime adapter](task-82-workspace-lambda-microvm-adapter.md)
4. [Task 83: Reconciliation and local fake-provider E2E](task-83-workspace-runtime-recovery-e2e.md)

Task 61's Workload Job adapter is not a Workspace runtime and does not satisfy
this parent task. Keep real AWS PoC, image/network proof, and deployment
approval as separate gates; EC2 remains out of scope.

The accepted Workspace contract is in [ADR 0009](../docs/adr/0009-workspace-lambda-microvm-lifecycle.md)
and [the runtime guide](../docs/workspace-lambda-microvm.md). Automatic
replacement stays disabled until durable Workspace storage (Task 44) and safe
agent/service recovery (Task 45) are available.

## Acceptance criteria

- Tasks 80–83 acceptance criteria pass and are individually documented/tested.
- Runtime adapter uses durable per-Workspace revision/tombstone fencing before
  provider effects, minimal IAM, and tags from `docs/aws-resource-tags.md`.
- Provider API integration runs against a local fake; no AWS apply is implicit.
- AWS image/container/network/lifecycle PoC remains explicitly unverified until
  approved and completed in a real Resource Plane account.
