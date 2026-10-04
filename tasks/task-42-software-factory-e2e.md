# Task 42: Software Factory End-to-End

Status: Blocked on Tasks 37–41 and 51. Priority: P0.

## Goal

Prove one Tenant-owned GitHub Repository can go from installation through one isolated Go test Job to a GitHub Check and retained redacted logs.

## Work

- Add a fake GitHub App/API test environment and a dedicated PostgreSQL schema fixture.
- Exercise installation binding, webhook signature/delivery idempotency, repository lookup, immutable SHA, Workload create/outbox/dispatch, scheduler consumer, worker, isolated Podman execution, artifact metadata, and Check update.
- Include duplicate webhook/SQS delivery, API idempotency replay, bad signature, wrong Tenant/repository, poisoned message/DLQ, cancellation, timeout, artifact/GitHub outage, and worker restart.
- Assert secrets never appear in DB, SQS payload, logs, Check body, or artifacts.
- Keep an opt-in real-AWS smoke test separate from PR CI; do not let test cleanup touch a shared/dev/prod account.

## Acceptance criteria

- Deterministic fake-backed E2E runs in CI using local PostgreSQL and Podman without public GitHub credentials.
- Assertions verify eventual Run state, Check conclusion, redacted log artifact, resource cleanup, and correct Tenant/requester visibility.
- Failure diagnostics include Hako Run/event IDs without credential material.
- AWS smoke prerequisites, dedicated account guard, required approvals, and cleanup report are documented.
