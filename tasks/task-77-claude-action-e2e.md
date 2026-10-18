# Task 77: Claude Action local E2E, operations and security evidence

Status: Todo (Task 76 now has focused durable Check delivery coverage; this task still needs the composed authorized webhook→runner→terminal Check→verified cleanup PostgreSQL E2E, negative trust/rotation/cancel/cleanup cases, CI target and operator/security runbook). Priority: P2. Parent: Task 73. Depends on: Tasks 74–76.

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
