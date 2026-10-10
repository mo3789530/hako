# Task 39: Idempotent Workload Execution Handler

Status: In Progress (durable Control Plane dispatch handoff underway). Priority: P0.

## Goal

Implement the short-lived dispatch, Resource Plane execution, and result/cleanup handlers specified by [ADR 0006](../docs/adr/0006-workload-worker-substrate.md). Reuse checkout, redaction, and artifact interfaces, but split the current direct-DB Service into Control Plane orchestration and Resource Plane executor. A valid queue message must never be treated as success merely because it decoded.

## Work

- Progress: added a transactional scheduler handoff that reloads the Run, recognizes terminal/duplicate deliveries, records a fixed placement and generation-scoped attempt, and writes a Resource Plane dispatch Outbox event. Scheduler defaults use batch size 1, 120-second visibility with 30-second renewal, and a five-minute processing cap; visibility renewal failure cancels handler work and leaves the receipt unacknowledged. Added a Control Plane result handler that fences Tenant/Run/attempt/generation/Resource Plane, ignores stale generations, handles duplicate/cancel/deadline races, and transactionally records terminal state plus redacted-log metadata. Result artifact metadata is capped at 100 MiB and SHA-256 validated. Resource Plane journal/controller, physical cleanup protocol, cancellation during launch, and full recovery remain outstanding.

- Implement a handler keyed by the versioned `(tenant_id, workload_run_id)` envelope; reload all authoritative fields from DSQL and ignore caller/message-supplied execution parameters.
- Acknowledge terminal duplicates without re-execution. Reject wrong Tenant/Run pairs, unsupported kinds, missing commit/repository, and cancelled Runs safely.
- Claim attempt/generation and dispatch Outbox atomically before side effects. Add conditional Resource Plane journal, fixed EC2 ClientToken/placement, stale-result rejection, and recovery that confirms old VM termination before replacement. Do not promise exactly-once arbitrary side effects or use a process-local cache as the correctness boundary.
- Use Task 38 checkout and existing redaction/artifact interfaces. Bound execution by immutable `timeout_at`; make cancellation reach the process/VM and track physical cleanup separately from logical terminal state. No Resource Plane DSQL connection.
- Classify permanent input/policy errors versus transient infrastructure errors; ensure retries do not strand a Run in `running` forever. Record safe error codes, not raw secrets or arbitrary subprocess output.

## Acceptance criteria

- PostgreSQL integration tests cover concurrent duplicate messages, duplicate after terminal state, cancellation before/during execution, crash/retry recovery, timeout, artifact failure, and Tenant mismatch.
- Scheduler message deletion occurs after durable attempt + dispatch Outbox (or terminal duplicate recognition); Resource Plane command deletion follows a recoverable acceptance checkpoint. Result deletion follows durable CP terminal/duplicate recognition. Never hold one receipt for 24 hours.
- Tests cover conditional-journal conflicts, cancel/launch races, stale generations, old-VM termination gating, visibility renewal failure, batch size 1, and fake-clock 24-hour deadlines.
- Log/artifact metadata transaction remains Tenant-scoped; object bytes are redacted before persistence.
- The handler works with injected checkout/executor/queue in tests and does not run untrusted commands on the host.
- Keep `HAKO_WORKLOAD_RUNS_ENABLED` disabled until Task 40 supplies a deployable worker and alarms.
