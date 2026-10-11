# Task 83: Workspace Runtime Reconciliation and Local API E2E

Status: Todo. Priority: P1. Parent: Task 43. Depends on: Tasks 80–82.

## Goal

Prove the Workspace Runtime's durable behavior across provider/API failures and
process restarts, without requiring an AWS account or taking ownership of
unrelated provider resources.

## Work

- Add a local fake provider HTTP API and exercise the actual runtime adapter
  through it.
- Reconcile a journal/provider mismatch using ownership tags and persisted
  Resource Bindings; never adopt a foreign or ambiguously tagged resource.
- Compensate partial provisioning; persist cleanup failures and retry them
  safely. Make delete idempotent.
- Test duplicate, stale, concurrent, partial-create, retry, tombstone, foreign
  resource, provider timeout, and controller restart scenarios.
- Document alarms, retry/operator procedures, provider quotas, lifecycle
  limitations, and the boundary between local proof and AWS PoC.

## Acceptance criteria

- Local fake-provider integration suite passes repeatedly and demonstrates no
  duplicate allocation, stale resurrection, or foreign-resource mutation.
- Focused test target is documented and runs without AWS credentials.
- Task 43 is marked locally implemented only after Tasks 80–83 pass; actual
  AWS MicroVM/container/network validation remains a distinct operational gate.
