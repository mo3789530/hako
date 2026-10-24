# Task 73: Claude Code Action on Hako Ephemeral Runners

Status: Local acceptance complete in Tasks 74–77: provenance-gated admission, Tenant-scoped credential resolver, signed guest bootstrap, runner execution, durable Checks, cleanup/retry, fake-provider PostgreSQL E2E, and a digest-pinned ARM64 runner image build are implemented. The image was inspected but not executed on this x86_64 host. Real MicroVM/GitHub/AWS integration and production rollout remain approval-gated; no model API or external account was used. Priority: P2. Parent: Task 52. Depends on: Tasks 40, 41, 50, 51, 52.

## Goal

Run repositories' existing Claude Code GitHub Action workflow steps on a one-job Hako Lambda MicroVM runner, with tenant-scoped credentials, trusted workflow policy and auditable cleanup.

## Work

- Document and test a supported workflow using the official Claude Code Action on an ephemeral Hako runner; do not silently translate user workflows into Hako Native Agent Jobs.
- Define credential modes and policy: prefer short-lived federated/provider credentials where supported; otherwise use a Tenant-scoped secret reference injected only into an approved trusted job. Never expose Hako/AWS credentials to repository code or logs.
- Deny model credentials to fork/untrusted pull requests, unapproved workflows, and jobs lacking explicit Tenant policy. Separate read-only review from branch/PR write permissions.
- Bind each run to Tenant, installation, repository, workflow ref, commit SHA and runner job ID. Capture redacted logs/check result, then deregister runner and destroy the MicroVM on success, failure, cancellation or timeout.
- Provide setup, secret rotation, disable and incident response documentation.

## Execution breakdown

- [Task 74: Workflow provenance and eligible-job contract](task-74-claude-action-provenance.md)
- [Task 75: Tenant model credential and trust policy](task-75-claude-tenant-credential-policy.md)
- [Task 76: Ephemeral execution and Check/result lifecycle](task-76-claude-action-execution.md)
- [Task 77: Local E2E, operations documentation and security evidence](task-77-claude-action-e2e.md)

Complete this parent only when Tasks 74–77 meet their acceptance criteria and an end-to-end fake/local run demonstrates both execution and cleanup. Real model API usage and external GitHub/AWS changes remain separately approved rollout steps.

## Acceptance criteria

- Fake GitHub/runner tests cover queued replay, wrong job/repository/SHA assignment, cancellation, timeout, registration failure, cleanup retry and credential rotation.
- Tests prove fork/untrusted jobs receive no model or cloud credential and trusted jobs cannot read another Tenant's secret.
- One runner executes at most one job; no credential remains in artifacts, logs or the next runtime.
- End-to-end local/fake test reports a GitHub Check and proves runner cleanup. Real Claude/API usage and external GitHub configuration are separate explicitly approved verification steps.
