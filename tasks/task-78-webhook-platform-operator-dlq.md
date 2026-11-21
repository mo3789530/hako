# Task 78: Platform Operator Webhook Dead-Letter Workflow

Status: Implemented locally (redacted cross-Tenant listing, verified platform-admin routes, active-policy replay, expiry/rate-limit checks, atomic platform audit, API contract and runbook; focused PostgreSQL/API integration test passes). Production operator-group provisioning and deployment remain separate gates. Priority: P0. Parent: Task 51. Depends on: existing webhook inbox/replay and platform-admin audit conventions.

## Goal

Give verified Hako platform operators a safe way to inspect and recover failed GitHub Webhook deliveries across Tenants without exposing raw payloads or bypassing Tenant authorization and current delivery eligibility.

## Work

- Add platform-admin-only, paginated failed-delivery inspection and an audited replay action. Reuse the verified Cognito `hako-admin` group gate used by platform Resource Plane health routes; Tenant Owner/Admin membership must not grant platform-wide access.
- Return only redacted metadata: Delivery ID, Tenant/Installation/Repository identity, event/action, received/processed timestamps, attempt/error category, payload-retention status, and replay eligibility. Never return raw payload, signature, token, or arbitrary upstream error text.
- Require a bounded operator reason for replay, preserve the original Delivery ID, enforce existing replay limits, and write actor/action/target/reason metadata to `platform_audit_events` atomically with the replay request.
- Revalidate current Tenant, Installation, Repository, event policy, and payload availability in the processor; operator replay must not revive deleted/suspended bindings or bypass Tenant policy.
- Define permission-denied behavior, pagination limits, rate limits, audit retention expectations, and operator runbook steps. Keep CLI access out of scope unless a non-interactive platform-admin authentication contract already exists.

## Acceptance criteria

- Unit and PostgreSQL integration tests prove unauthenticated, ordinary user, and Tenant-only Owner/Admin callers cannot inspect or replay platform-wide deliveries; only a verified `hako-admin` claim can.
- Cross-Tenant results are paginated, redacted, and contain no raw payload or secret material.
- Replay is idempotent, preserves Delivery ID, records operator and reason in the same transaction, and is rejected for expired payloads or inactive/deleted bindings.
- Concurrent operator replay attempts cannot create duplicate processing claims or bypass per-delivery limits.
- API contract and operator runbook document audit, eligibility, retry, and incident-response behavior.
