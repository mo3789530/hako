# Task 83: Workspace Runtime Reconciliation and Local API E2E

Status: Implemented locally (AWS SDK HTTP fake provider lifecycle E2E, process-restart retry with frozen request, foreign provider ID fail-closed tests, cleanup failure/retry fencing, concurrent duplicate ensure, provider timeout recovery, and Resource Controller→result-consumer→PostgreSQL binding E2E pass). Production AWS lifecycle/resource ownership PoC remains unverified. Priority: P1. Parent: Task 43. Depends on: Tasks 80–82.

## Goal

Prove the Workspace Runtime's durable behavior across provider/API failures and
process restarts, without requiring an AWS account or taking ownership of
unrelated provider resources.

## Work

- Add a local fake provider HTTP API and exercise the actual runtime adapter
  through it.
- Reconcile a journal/provider mismatch using the journal's persisted provider
  ID and verify provider configuration. Never adopt a foreign resource. Task 80
  found no verified per-MicroVM tag or ownership-list API, so tag discovery and
  adoption are explicitly unsupported; use the journal as the binding record.
- Compensate partial provisioning; persist cleanup failures and retry them
  safely. Make delete idempotent.
- Test duplicate, stale, concurrent, partial-create, retry, tombstone, foreign
  resource, provider timeout, and controller restart scenarios.
- Document alarms, retry/operator procedures, provider quotas, lifecycle
  limitations, and the boundary between local proof and AWS PoC.

## Local progress

`internal/workspaceruntime/lambda_http_test.go` drives the actual AWS SDK client
through a loopback HTTP fake. It covers ensure/suspend/resume/delete, duplicate
ensure, an ambiguous first launch response followed by adapter reconstruction,
and proof that a mismatched provider ID is rejected without deletion. The
operator boundaries are recorded in
[`docs/workspace-runtime-operations.md`](../docs/workspace-runtime-operations.md).

The local Task 83 acceptance suite is complete. Its command/result queue is an
in-process test double, not live SQS. Production AWS lifecycle and resource
ownership remain unverified and must not be inferred from the local suite.

## Acceptance criteria

- Local fake-provider integration suite passes repeatedly and demonstrates no
  duplicate allocation, stale resurrection, or foreign-resource mutation.
- Focused test target is documented and runs without AWS credentials.
- Task 43 is marked locally implemented only after Tasks 80–83 pass; actual
  AWS MicroVM/container/network validation remains a distinct operational gate.
