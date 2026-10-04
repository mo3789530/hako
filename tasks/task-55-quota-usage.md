# Task 55: Workload/Service Quotas and Usage UX

Status: Blocked on Tasks 40, 47, and 50. Priority: P3.

## Goal

Protect Tenant and platform capacity and make expected resource use visible before a Job, Service, or Workspace starts.

## Work

- Define limits for concurrent Workload Runs, compute class, queue backlog, service memory/CPU, artifact bytes, and preview lifetime.
- Add transactional admission/reservation and release-on-terminal/cleanup semantics; do not trust user-reported actual usage as authoritative billing data.
- Add Tenant Owner/Admin APIs for quota configuration and read-only usage views with pagination.
- Show validation, current usage, estimated Runtime class, and safe confirmation in CLI without exposing other Tenant usage.

## Acceptance criteria

- Concurrent integration tests prove limits cannot be exceeded under OCC/transaction retries and reservations release exactly once.
- Tests cover canceled-before-start, failed provisioning, timeout, orphan cleanup, and duplicate terminal events.
- Tenant members cannot read or modify other Tenant quotas/usage; sensitive billing identifiers are not exposed.
- Document configured quota versus measured usage and explicitly defer billing until a separate ADR.
