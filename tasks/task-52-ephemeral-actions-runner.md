# Task 52: GitHub Actions Ephemeral Runner Mode

Status: In Progress (2026-10-17: signed `workflow_job.queued` deliveries are retained but excluded from the generic push/PR Go-test trigger. Normalized events preserve run attempt, workflow name, bounded labels, and optional assigned runner/group identity. Tenant Owner/Admin repository opt-in is stored via API/CLI and audited. A separate default-off admission flag now revalidates Tenant/Installation/Repository policy and persists deduplicated queued job admissions. Registration-token minting, exact runner assignment, Lambda MicroVM lifecycle, and cleanup remain unimplemented). Priority: P3.

Claude Code Action support is tracked separately in [Task 73](task-73-claude-code-action.md), including Tenant-scoped model credentials, untrusted PR denial, and action-specific E2E.

## Goal

Offer Hako as an ephemeral self-hosted runner backend for existing GitHub Actions workflows without taking ownership of or rewriting user workflow YAML.

## Work

- Consume only authorized `workflow_job.queued` events and correlate repository, workflow, ref, commit, labels, Tenant policy, and Check state.
- Keep the per-repository `actions_runner_enabled` policy separate from Factory event triggers; an opt-in must never itself issue a token or create runtime resources.
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
- `WorkflowJobRef` now retains the workflow/job/run IDs, positive `run_attempt`
  when provided, workflow name, exact bounded runner label strings (maximum
  100), and optional runner/group assignment IDs and names. Action/status
  mismatches, invalid label controls/lengths, and non-positive supplied
  IDs/attempts fail normalization.
  Queued jobs with null runner assignment remain unassigned; later events can
  be correlated against the exact job and run attempt. Older normalized rows
  remain readable because these fields are additive/optional.
- `HAKO_GITHUB_ACTIONS_RUNNER_ADMISSIONS_ENABLED` defaults off. When enabled,
  the webhook processor admits only queued jobs whose current Tenant-owned
  Installation and Repository opt-in were active before receipt. Admission is
  idempotent in `github_actions_runner_jobs` and does not issue credentials or
  launch compute. A dedicated controller is required to drain the table.
