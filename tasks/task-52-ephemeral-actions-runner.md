# Task 52: GitHub Actions Ephemeral Runner Mode

Status: In Progress (2026-10-17: signed `workflow_job.queued` deliveries are retained but excluded from the generic push/PR Go-test trigger. Dedicated runner admission must authorize the event before requesting a registration token or creating a MicroVM. Runner lifecycle is not implemented). Priority: P3.

Claude Code Action support is tracked separately in [Task 73](task-73-claude-code-action.md), including Tenant-scoped model credentials, untrusted PR denial, and action-specific E2E.

## Goal

Offer Hako as an ephemeral self-hosted runner backend for existing GitHub Actions workflows without taking ownership of or rewriting user workflow YAML.

## Work

- Consume only authorized `workflow_job.queued` events and correlate repository, workflow, ref, commit, labels, Tenant policy, and Check state.
- Register one ephemeral runner for one job, wait for the exact assigned job, capture diagnostics, deregister it, and destroy the isolated execution environment on every terminal path.
- Apply trust policy: fork PR has no secrets/cloud role; privileged labels require explicit policy/approval; no AWS credentials are injected as long-lived keys.
- Implement bounded runner registration token lifetime, capacity, retry, queue timeout, and GitHub API throttling.

## Acceptance criteria

- Fake GitHub tests cover duplicate queued events, runner assignment mismatch, startup failure, cancellation, timeout, job completion, deregistration failure, and cleanup retry.
- Tests prove one runner accepts at most one job and never processes a second repository/job.
- Runner logs are collected/redacted before ephemeral disk deletion and attached to the correct commit Check.
- Permissions are separately reviewed from ordinary Repository checkout and scoped to the minimum Actions/Administration APIs required.

## Progress

- The shared webhook inbox continues to normalize and retain `workflow_job`
  deliveries for the dedicated runner path, but generic Repository
  verification Runs now accept only push/pull-request events. A queued Actions
  job therefore cannot accidentally start Hako's default `go test ./...`
  workload. The future runner admission path must independently resolve
  Tenant/Repository policy; no runner token, runtime, or Check is created by
  the generic trigger.
