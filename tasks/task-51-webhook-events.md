# Task 51: GitHub Webhook Event Lifecycle and Operations

Status: Implemented locally (Tasks 78–79 operator API/runbook and lifecycle PostgreSQL E2E complete; 2026-10-11). Production operator provisioning, AWS deployment, and downstream MicroVM worker enablement remain separate gates. Signature verification, versioned normalization for push/pull_request/workflow_job, Tenant-bound inbox and Installation lifecycle processor, seven-day raw-payload purge, Owner/Admin replay and redacted listing, per-delivery processor fencing, Delivery ID-idempotent Run/Check/Outbox, default-off repository triggers, and platform operator replay are locally verified. Priority: P0.

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

## Replay API error-contract coverage (2026-10-25)

The PostgreSQL API integration test also verifies malformed JSON, missing,
short, and over-limit replay reasons return `400 invalid_request`; an
integration ID owned by another Tenant is hidden as `404`; and exhausting the
per-delivery replay budget returns `429` with `Retry-After: 3600`. These cases
use the real local API/store path and synthetic identities; they do not call
GitHub or AWS.
