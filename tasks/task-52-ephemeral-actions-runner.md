# Task 52: GitHub Actions Ephemeral Runner Mode

Status: Blocked on Tasks 40 and 41. Priority: P3.

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
