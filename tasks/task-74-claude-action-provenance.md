# Task 74: Claude Action workflow provenance and eligible-job contract

Status: In Progress (a fail-closed verifier, repository-scoped GitHub REST Run/Job lookup, and Owner/Admin policy API/CLI with audited PostgreSQL persistence are implemented locally. The lookup and verifier are not yet connected before MicroVM startup or to the admitted job identity). Priority: P2. Parent: Task 73. Depends on: Tasks 51, 52.

## Goal

Treat the repository's explicit Claude Code Action workflow as an ordinary GitHub Actions job on Hako's ephemeral runner, and bind every launch to the exact authorized workflow/job identity. Hako must not rewrite or infer a workflow from an issue or PR comment.

## Work

- Define the allowed event, repository installation, workflow path/ref, commit SHA, workflow/job run IDs, runner labels and Tenant policy inputs.
- [x] Define a data-only allowlist contract and pure verifier. Forks and `pull_request_target`-style unapproved events fail closed; policy cannot specify commands, credentials, images, roles or network profiles.
- [x] Add Tenant App repository-scoped `actions:read` Installation token issuance with the runner opt-in rechecked before and after minting. No contents, write, administration, registration, or workflow-dispatch permission is included.
- [x] Add bounded fake-testable GitHub REST lookup for exact Run, attempt, Job and workflow path. Redirects, oversized responses, missing/ambiguous jobs, wrong repository/run/attempt, and malformed workflow metadata fail closed.
- [x] Persist the per-Tenant/repository allowlist through an Owner/Admin API and CLI, audit changes, and clear it transactionally when runner opt-in is disabled.
- Verify webhook signature/delivery replay and fetch authoritative workflow/job metadata before provisioning a runner.
- [ ] Wire authoritative GitHub lookup + persisted policy + exact queued identity verification before claim consumption/MicroVM creation; reject stale, duplicate, fork-originated or mismatched work before creating runtime resources.
- Document supported Claude Code Action versions/configuration as user-authored workflow steps, including which Hako runner labels to request.

## Acceptance criteria

- Fake GitHub tests reject wrong Tenant/repository/installation/workflow/ref/SHA/job ID, replay and fork PR before runtime creation.
- Duplicate queued deliveries create at most one run; retries return the same run identity.
- No issue/comment text can directly select arbitrary commands, credentials, workflow files or runner privilege.
- Workflow contract and unsupported cases are documented without changing existing GitHub Actions YAML semantics.
