# Task 51: GitHub Webhook Event Lifecycle and Operations

Status: In Progress; [Task 78](task-78-webhook-platform-operator-dlq.md) is implemented locally, and [Task 79](task-79-webhook-lifecycle-e2e.md) remains for full lifecycle E2E. Signature verification, versioned normalization for push/pull_request/workflow_job, Tenant-bound inbox and Installation lifecycle processor already exist. Seven-day raw-payload purge, Owner/Admin replay endpoint, redacted failed-delivery API/CLI listing, per-delivery concurrent processor fencing, idempotent Delivery ID→Workload Run persistence, default-off per-repository push/PR trigger scheduling, and platform operator replay are implemented locally with focused PostgreSQL tests. Priority: P0.

## Goal

Complete the durable GitHub Webhook path beyond the already-supported signature validation, delivery inbox, and Installation lifecycle synchronization.

## Work

- Normalize supported `push`, `pull_request`, and later `workflow_job` deliveries into versioned Hako Repository Events tied to an active Tenant Installation/Repository binding.
- Add actor/event audit metadata, bounded retention for raw payloads, explicit redaction, and idempotent event-to-Run creation.
- Add operator-safe dead-letter/replay tooling that reuses the original Delivery ID and records replay actor/reason; prevent replay from bypassing current Tenant state, permissions, or policy. Tenant Owner/Admins can list redacted failed Tenant-owned deliveries and request audited replay; a platform operator dead-letter workflow remains.
- Reject unsupported event/action combinations early and preserve raw-payload signature-before-parse rules.

## Acceptance criteria

- Integration tests cover duplicate Delivery ID, repository removed/suspended after receipt, stale replay, malformed payload, signature mismatch, event-version migration, and concurrent processor claims. Processing transactions lock/revalidate the delivery row; Aurora DSQL OCC conflicts are retried by the shared transaction runner.
- Raw payload retention is bounded and secrets/tokens are redacted; default API responses never expose raw payload.
- Raw signed payloads are purged after seven days while normalized event, actor, hash and processing/audit metadata remain. The scheduled processor runs bounded cleanup batches.
- Replay is authenticated Owner/Admin-only, audited, rate limited to three requests per delivery per hour, preserves the Delivery ID, requires a currently active Tenant App/Installation binding, and is revalidated again by the processor.
- Event-to-Workload creation uses a stable Delivery ID idempotency key, reloads normalized identity from the authorized inbox and transactionally creates Run/Check/Outbox before enabling workflow-job scheduling. Task 52 still owns runner scheduling and workflow provenance checks.
- Repository trigger scheduling is Owner/Admin opt-in, default-off, and must suppress inbox deliveries received before the repository's latest opt-in timestamp. It creates only fixed `untrusted` `standard` verification Jobs for push and PR opened/reopened/synchronize events; it does not execute `workflow_job`.
- Runbook documents failed deliveries, replay eligibility, retention, and incident response.
