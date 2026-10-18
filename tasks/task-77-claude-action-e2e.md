# Task 77: Claude Action local E2E, operations and security evidence

Status: In Progress (the focused local/CI PostgreSQL lifecycle starts with a rejected tampered signature, then uses the production Tenant webhook HTTP handler and DB-backed Installation authorizer for signed queued, runner-assignment and terminal deliveries. It admits and leases the job, runs the startup worker with fake Lambda MicroVM/token/bootstrap providers, dispatches all durable Check Outbox revisions through the production Dispatcher/Checks consumer to a fake GitHub Checks API, then recovers Broker-mediated physical cleanup after each provider fails once. Success/failure/cancel/timeout, duplicate terminal deliveries, signature rejection and lease fencing are covered. Cross-Tenant credential denial, wrong SHA/job, credential rotation/revocation E2E and a full Tenant setup/security runbook remain). Priority: P2. Parent: Task 73. Depends on: Tasks 74–76.

## Goal

Provide a repeatable local/fake end-to-end proof and operator documentation for the Claude Code Action path without requiring real Claude API usage or modifying GitHub/AWS configuration.

## Work

- [x] Compose authorized queued admission → runner startup/bootstrap → GitHub assignment/completion → durable Check → verified retryable cleanup using local PostgreSQL and fake providers.
- [x] Compose signed HTTP webhook ingress and outgoing fake-GitHub Checks API publication into that same end-to-end test; verify a tampered signature cannot enter the inbox.
- Add negative E2E cases for fork/untrusted PR, cross-Tenant secret access, wrong SHA/job, credential rotation/revocation, cancellation and cleanup failure.
- [ ] Complete Tenant setup, supported workflow configuration, credential rotation/disable, audit evidence and incident-response runbook. The existing runtime and cleanup docs already distinguish local evidence from AWS/GitHub deployment gates.
- Add CI invocation and retain only redacted, non-secret test evidence.

## Acceptance criteria

- One command runs the local E2E against fake external services and PostgreSQL without stopping the shared local database.
- Tests prove no secret is resolved for denied jobs and no credential, runner or MicroVM remains after terminal completion or retryable cleanup failure.
- Security documentation distinguishes tested local behavior from unverified AWS IAM, Lambda MicroVM image/network behavior and external GitHub settings.
- Real model API calls, live GitHub installation changes, secret provisioning and AWS apply remain out of scope unless explicitly approved.
