# Task 74: Claude Action workflow provenance and eligible-job contract

Status: In Progress (a fail-closed, fake-testable provenance verifier now compares Tenant/integration/Installation/repository, workflow path/ref, SHA, run/job/attempt, event, head repository, fork status and required labels. It is not yet connected to a persisted Tenant policy or authoritative GitHub Run/Job API lookup). Priority: P2. Parent: Task 73. Depends on: Tasks 51, 52.

## Goal

Treat the repository's explicit Claude Code Action workflow as an ordinary GitHub Actions job on Hako's ephemeral runner, and bind every launch to the exact authorized workflow/job identity. Hako must not rewrite or infer a workflow from an issue or PR comment.

## Work

- Define the allowed event, repository installation, workflow path/ref, commit SHA, workflow/job run IDs, runner labels and Tenant policy inputs.
- [x] Define a data-only allowlist contract and pure verifier. Forks and `pull_request_target`-style unapproved events fail closed; policy cannot specify commands, credentials, images, roles or network profiles.
- Verify webhook signature/delivery replay and fetch authoritative workflow/job metadata before provisioning a runner.
- Require the queued job to match the approved repository, workflow ref, SHA, Tenant, installation and configured Claude Action policy; reject stale, duplicate, fork-originated or mismatched work before creating runtime resources.
- Document supported Claude Code Action versions/configuration as user-authored workflow steps, including which Hako runner labels to request.

## Acceptance criteria

- Fake GitHub tests reject wrong Tenant/repository/installation/workflow/ref/SHA/job ID, replay and fork PR before runtime creation.
- Duplicate queued deliveries create at most one run; retries return the same run identity.
- No issue/comment text can directly select arbitrary commands, credentials, workflow files or runner privilege.
- Workflow contract and unsupported cases are documented without changing existing GitHub Actions YAML semantics.
