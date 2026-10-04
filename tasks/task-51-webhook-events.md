# Task 51: GitHub Webhook Event Lifecycle and Operations

Status: Blocked on Tasks 37 and 39. Priority: P0.

## Goal

Complete the durable GitHub Webhook path beyond the already-supported signature validation, delivery inbox, and Installation lifecycle synchronization.

## Work

- Normalize supported `push`, `pull_request`, and later `workflow_job` deliveries into versioned Hako Repository Events tied to an active Tenant Installation/Repository binding.
- Add actor/event audit metadata, bounded retention for raw payloads, explicit redaction, and idempotent event-to-Run creation.
- Add operator-safe dead-letter/replay tooling that reuses the original Delivery ID and records replay actor/reason; prevent replay from bypassing current Tenant state, permissions, or policy.
- Reject unsupported event/action combinations early and preserve raw-payload signature-before-parse rules.

## Acceptance criteria

- Integration tests cover duplicate Delivery ID, repository removed/suspended after receipt, stale replay, malformed payload, signature mismatch, event-version migration, and concurrent processor claims.
- Raw payload retention is bounded and secrets/tokens are redacted; default API responses never expose raw payload.
- Replay is authenticated/admin-only, audited, rate limited, and cannot duplicate a Workload because of stable idempotency keys.
- Runbook documents failed deliveries, replay eligibility, retention, and incident response.
