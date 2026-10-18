# Task 77: Claude Action local E2E, operations and security evidence

Status: In Progress (the focused local/CI PostgreSQL lifecycle now starts from an opted-in repository's authorized queued delivery, admits and leases the job, runs the startup worker with fake Lambda MicroVM/token/bootstrap providers, applies GitHub runner assignment and completion deliveries, verifies the durable terminal Check, and recovers Broker-mediated physical cleanup after each provider fails once. Success/failure/cancel/timeout, duplicate terminal deliveries and lease fencing are covered. The composed test injects `VerifiedDelivery`; HTTP signature verification and outgoing fake-GitHub Checks API are covered by separate focused integrations, not yet by this single end-to-end test. Cross-Tenant credential denial, wrong SHA/job, rotation/revocation E2E and the complete Tenant setup/security runbook remain). Priority: P2. Parent: Task 73. Depends on: Tasks 74–76.

## Goal

Provide a repeatable local/fake end-to-end proof and operator documentation for the Claude Code Action path without requiring real Claude API usage or modifying GitHub/AWS configuration.

## Work

- [x] Compose authorized queued admission → runner startup/bootstrap → GitHub assignment/completion → durable Check → verified retryable cleanup using local PostgreSQL and fake providers.
- [ ] Compose signed HTTP webhook ingress and outgoing fake-GitHub Checks API publication into that same end-to-end test.
- Add negative E2E cases for fork/untrusted PR, cross-Tenant secret access, wrong SHA/job, credential rotation/revocation, cancellation and cleanup failure.
- Document Tenant setup, supported workflow configuration, credential rotation/disable, audit evidence, incident response and explicit production rollout prerequisites.
- Add CI invocation and retain only redacted, non-secret test evidence.

## Acceptance criteria

- One command runs the local E2E against fake external services and PostgreSQL without stopping the shared local database.
- Tests prove no secret is resolved for denied jobs and no credential, runner or MicroVM remains after terminal completion or retryable cleanup failure.
- Security documentation distinguishes tested local behavior from unverified AWS IAM, Lambda MicroVM image/network behavior and external GitHub settings.
- Real model API calls, live GitHub installation changes, secret provisioning and AWS apply remain out of scope unless explicitly approved.
