# Task 50: Security, Recovery, and Production Operations

Status: Blocked on Tasks 40, 42, and 49. Priority: P2.

## Goal

Close cross-cutting security and operational gaps before production or broader Tenant onboarding.

## Work

- Review Tenant isolation, Hako RBAC, session replay, duplicate/out-of-order Operations, webhook replay, queue poison messages, secrets, artifact access, and untrusted fork PRs.
- Define DSQL backup/restore, migration rollback/forward fix, Outbox retention, artifact retention/deletion, Workspace cleanup, orphan AWS discovery, and DLQ redrive procedures.
- Add structured correlation IDs from webhook/Operation/Run through Queue, Runtime, logs, artifacts, and GitHub Checks; redact secrets and minimize developer data in logs.
- Establish metrics/SLO alarms for API, DSQL, Outbox lag, queue age/DLQ, worker capacity/failure, Gateway, Runtime, and cleanup backlog.
- Record deferred BYOC, multi-region, persistent database, and billing requirements as independent ADRs with owners and non-goals.

## Acceptance criteria

- Threat-model review has explicit assets, trust boundaries, abuse cases, mitigations, and accepted residual risks.
- Recovery/cleanup drills run against disposable local/fake data and prove idempotence; AWS drills require explicit approval and isolated accounts.
- CI includes Go tests, PostgreSQL integration tests, Podman-isolated workload tests, Terraform fmt/validate, and security/policy checks.
- Operator runbooks include alerts, diagnosis, safe retry/redrive, restore, rollback, and escalation instructions.
