# Task 77: Claude Action local E2E, operations and security evidence

Status: In Progress (focused local/CI lifecycle command, terminal webhook→PostgreSQL cleanup-worker recovery tests for success/failure/cancel/timeout, stale-lease rejection and cleanup recovery notes are added alongside Task 76 Check delivery coverage. The complete authorized webhook→runner→terminal Check→verified cleanup PostgreSQL E2E, negative trust/rotation cases, and the full Tenant setup/security runbook remain). Priority: P2. Parent: Task 73. Depends on: Tasks 74–76.

## Goal

Provide a repeatable local/fake end-to-end proof and operator documentation for the Claude Code Action path without requiring real Claude API usage or modifying GitHub/AWS configuration.

## Work

- Add a local PostgreSQL + fake GitHub/runner/provider E2E that starts from an authorized workflow job and ends with a GitHub Check plus verified cleanup.
- Add negative E2E cases for fork/untrusted PR, cross-Tenant secret access, wrong SHA/job, credential rotation/revocation, cancellation and cleanup failure.
- Document Tenant setup, supported workflow configuration, credential rotation/disable, audit evidence, incident response and explicit production rollout prerequisites.
- Add CI invocation and retain only redacted, non-secret test evidence.

## Acceptance criteria

- One command runs the local E2E against fake external services and PostgreSQL without stopping the shared local database.
- Tests prove no secret is resolved for denied jobs and no credential, runner or MicroVM remains after terminal completion or retryable cleanup failure.
- Security documentation distinguishes tested local behavior from unverified AWS IAM, Lambda MicroVM image/network behavior and external GitHub settings.
- Real model API calls, live GitHub installation changes, secret provisioning and AWS apply remain out of scope unless explicitly approved.
