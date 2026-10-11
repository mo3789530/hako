# Task 79: Webhook Retention, Replay, and Trigger End-to-End Tests

Status: Todo. Priority: P0. Parent: Task 51. Depends on: Tasks 51 and 78.

## Goal

Prove the complete webhook delivery lifecycle locally, including retention, Tenant and platform-operator replay, default-off repository triggers, and idempotent Workload creation, using PostgreSQL and fake GitHub services only.

## Work

- Add a repeatable integration/E2E scenario from signed webhook receipt through normalization, delivery processing, failed-delivery listing/replay, Run/Check/Outbox creation, and terminal acknowledgment.
- Cover both Tenant Owner/Admin and platform `hako-admin` paths, including their separate visibility and audit records.
- Exercise duplicate and concurrent delivery/replay, stale or expired payload, seven-day purge, inactive/deleted Installation or Repository, trigger disabled/enabled and pre-opt-in delivery suppression, unsupported event/action, malformed/tampered signature, and transaction retry behavior.
- Assert the Run identity comes only from the verified normalized inbox row, preserves Delivery ID idempotency, and is created only for the fixed untrusted verification job types permitted by Task 51.
- Add one documented command/CI target that uses the already-running local PostgreSQL service or an isolated schema; it must not stop or mutate unrelated schemas.

## Acceptance criteria

- Integration/E2E tests cover successful processing, replay and duplicate races, retention expiry, opt-in boundary, and current-authorization revalidation.
- No test/API/CLI output contains raw webhook payload, signature, token, or unredacted secret values.
- Repeated webhook and replay attempts yield at most one matching Run, Check, and Outbox operation while preserving audit history.
- `go test ./...` and the focused PostgreSQL integration command pass; the test command and expected fixture behavior are documented.
- Documentation distinguishes local/fake verification from unperformed AWS deployment and external GitHub configuration.
